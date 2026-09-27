/**
 * fragment_map_test.mjs — the fragment half of the tsserver-hook worker's map.
 *
 *   node fragment_map_test.mjs <tsserver-hook-worker.js> <leaf fragment> <root fragment>
 *
 * The two fragment arguments are the real bytes tsconfig_aspect wrote for
 * //tests/lsp/fragment_fixture, so the producer and the consumer of the format
 * are checked against each other rather than against a hand-written fixture.
 * The `leaf` one is the point of the whole mechanism: that target is
 * `//visibility:private`, so no rule -- and therefore nothing in
 * .bazel/tsserver-hook-data.json -- can name it.
 *
 * Two workspaces are staged. The first has fragments and checks what they add,
 * what a second configuration must not double-count, and what a stale one must
 * not contribute. The second has none, and checks that the data file alone still
 * resolves: the .bazelrc lines that produce fragments are optional, and a
 * consumer without them must be no worse off than before they existed.
 */

import { Worker } from 'node:worker_threads';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const [workerPath, leafFragment, rootFragment] = process.argv.slice(2);
for (const arg of [workerPath, leafFragment, rootFragment]) {
  if (!arg || !fs.existsSync(arg)) {
    process.stderr.write(`FATAL: not found: ${arg}\n`);
    process.exit(1);
  }
}

const FIXTURE_PKG = 'tests/lsp/fragment_fixture';
const SUFFIX = '.tsconfig-fragment.json';

let failures = 0;
const pass = (name) => process.stdout.write(`PASS: ${name}\n`);
const fail = (name, detail) => {
  process.stderr.write(`FAIL: ${name}${detail ? ': ' + detail : ''}\n`);
  failures += 1;
};

function newRoot(tag) {
  return fs.mkdtempSync(path.join(process.env.TEST_TMPDIR || os.tmpdir(), `${tag}-`));
}

function write(root, rel, contents) {
  const p = path.join(root, rel);
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, contents);
  return p;
}

/** A fragment file at the path a build of `label` in `config` would put it. */
function stage(root, config, label, lines) {
  const [pkg, name] = label.replace(/^@+/, '').replace(/^\/\//, '').split(':');
  return write(root, `bazel-out/${config}/bin/${pkg}/${name}${SUFFIX}`, lines.join('\n') + '\n');
}

function fragmentLines(file) {
  return fs.readFileSync(file, 'utf8').split('\n').filter((l) => l.trim());
}

// ── The workspace the fragments describe ─────────────────────────────────────

const withFragments = newRoot('fragments');

write(withFragments, 'MODULE.bazel', 'module(name = "fixture")\n');
write(withFragments, 'BUILD.bazel', '');

// What the aspect said about //tests/lsp/fragment_fixture, on disk. The package
// is a Bazel package (it has a BUILD file) and has an entry point, which is what
// the worker resolves it to.
write(withFragments, `${FIXTURE_PKG}/BUILD.bazel`, '');
const fixtureIndex = write(withFragments, `${FIXTURE_PKG}/index.ts`, 'export const leaf = 1;\n');

// A package only .bazel/tsserver-hook-data.json names. It must survive: the
// fragments augment that file, they do not replace it.
write(withFragments, 'src/from_data/BUILD.bazel', '');
const fromDataIndex = write(withFragments, 'src/from_data/index.ts', 'export const d = 1;\n');
write(
  withFragments,
  '.bazel/tsserver-hook-data.json',
  JSON.stringify({ packages: ['src/from_data'] })
);

const leafLines = fragmentLines(leafFragment);
const rootLines = fragmentLines(rootFragment);
process.stdout.write(`INFO: leaf fragment =\n${leafLines.join('\n')}\n`);
process.stdout.write(`INFO: root fragment =\n${rootLines.join('\n')}\n`);

stage(withFragments, 'k8-fastbuild', `//${FIXTURE_PKG}:leaf`, leafLines);
stage(withFragments, 'k8-fastbuild', `//${FIXTURE_PKG}:root`, rootLines);

// The same label under a second configuration: counted once, the sorted roots
// deciding which wins rather than the filesystem.
stage(withFragments, 'k8-opt', `//${FIXTURE_PKG}:root`, rootLines);

// A fragment whose package is gone from the source tree. Discovery never looks
// in a directory the source tree does not have, so nothing here is even read.
stage(withFragments, 'k8-fastbuild', '//deleted/pkg:target', [
  JSON.stringify({ format: 'tsconfig-fragment-v1', label: '@@//deleted/pkg:target' }),
  JSON.stringify({ package: 'src/from_data', index: true }),
]);

// A fragment in a package that does exist, naming a package that does not: a
// removed package lingers in bazel-out until that target is rebuilt.
stage(withFragments, 'k8-fastbuild', `//${FIXTURE_PKG}:renamed`, [
  JSON.stringify({ format: 'tsconfig-fragment-v1', label: `@@//${FIXTURE_PKG}:renamed` }),
  JSON.stringify({ package: 'src/deleted_by_a_later_commit', index: true }),
]);

// A format this worker does not understand: skipped whole, not half-read.
stage(withFragments, 'k8-fastbuild', `${FIXTURE_PKG}:future`, [
  JSON.stringify({ format: 'tsconfig-fragment-v99', label: `@@//${FIXTURE_PKG}:future` }),
  JSON.stringify({ package: 'src/future_only', index: true }),
]);
write(withFragments, 'src/future_only/BUILD.bazel', '');
write(withFragments, 'src/future_only/index.ts', 'export const f = 1;\n');

// ── The workspace without them ──────────────────────────────────────────────

const noFragments = newRoot('nofragments');
write(noFragments, 'MODULE.bazel', 'module(name = "fixture")\n');
write(noFragments, 'BUILD.bazel', '');
write(noFragments, 'src/from_data/BUILD.bazel', '');
const fallbackIndex = write(noFragments, 'src/from_data/index.ts', 'export const d = 1;\n');
write(
  noFragments,
  '.bazel/tsserver-hook-data.json',
  JSON.stringify({ packages: ['src/from_data'] })
);

// ── Assertions ──────────────────────────────────────────────────────────────

function expectEntry(map, key, want) {
  if (!(key in map)) {
    fail(`${key} is in the map`, `keys: ${JSON.stringify(Object.keys(map).sort())}`);
  } else if (map[key] !== want) {
    fail(`${key} resolves correctly`, `got ${map[key]}, want ${want}`);
  } else if (!fs.existsSync(map[key])) {
    fail(`${key} points at a real path`, map[key]);
  } else {
    pass(`${key} -> ${map[key]}`);
  }
}

function expectAbsent(map, key, why) {
  if (key in map) {
    fail(`${key} must NOT be in the map (${why})`, `got ${map[key]}`);
  } else {
    pass(`${key} is absent (${why})`);
  }
}

/** The map the worker posts for `root`, and everything it logged getting there. */
function runWorker(root) {
  return new Promise((resolve, reject) => {
    const worker = new Worker(workerPath, {
      workerData: { workspaceRoot: root },
      env: { ...process.env, TSSERVER_HOOK_DEBUG: '1' },
      stderr: true,
    });
    let log = '';
    let map = null;
    let stopping = false;
    const startupLogged = () =>
      map && log.includes(`initial resolution map: ${Object.keys(map).length} entries\n`);
    const stopWhenReady = () => {
      if (stopping || !startupLogged()) return;
      stopping = true;
      worker.terminate();
    };
    worker.stderr.on('data', (chunk) => {
      log += chunk;
      stopWhenReady();
    });
    const timeout = setTimeout(() => {
      worker.terminate();
      reject(new Error(`worker sent no resolution map within 60s for ${root}`));
    }, 60000);
    worker.on('error', (err) => {
      reject(err);
    });
    worker.once('message', (msg) => {
      if (msg.type !== 'resolution-map') {
        reject(new Error(`unexpected message type ${msg.type}`));
        worker.terminate();
        return;
      }
      map = msg.data;
      stopWhenReady();
    });
    // The final startup record follows the fragment count; termination can truncate later stderr.
    worker.on('exit', () => {
      clearTimeout(timeout);
      if (startupLogged()) resolve({ map, log });
      else reject(new Error(`worker exited without its initial map and startup log for ${root}`));
    });
  });
}

const withRun = await runWorker(withFragments);
const withMap = withRun.map;
process.stdout.write(`INFO: map with fragments = ${JSON.stringify(withMap, null, 2)}\n`);
process.stdout.write(`INFO: worker log =\n${withRun.log}\n`);

// The headline: a package no rule may name, resolved from its fragment alone.
expectEntry(withMap, FIXTURE_PKG, fixtureIndex);

// The data file is still the data file.
expectEntry(withMap, 'src/from_data', fromDataIndex);

// Five fragment files, three labels: :root under two configurations counts
// once, :future's format is rejected whole, //deleted/pkg is never opened.
const counted = 'fragments: 3 labels from 5 files';
if (withRun.log.includes(counted)) {
  pass(`the merge deduped by label (${counted})`);
} else {
  fail(`the merge deduped by label (${counted})`, 'not in the worker log');
}

expectAbsent(withMap, 'src/deleted_by_a_later_commit', 'the package it names no longer exists');
expectAbsent(withMap, 'src/future_only', 'the fragment declares a format this worker cannot read');

const withoutMap = (await runWorker(noFragments)).map;
process.stdout.write(`INFO: map without fragments = ${JSON.stringify(withoutMap, null, 2)}\n`);

// The fallback the .bazelrc lines are optional against.
expectEntry(withoutMap, 'src/from_data', fallbackIndex);
expectAbsent(withoutMap, FIXTURE_PKG, 'no build wrote a fragment for it');

if (failures > 0) {
  process.stderr.write(`\n${failures} FAILED\n`);
  process.exit(1);
}
process.stdout.write('\nALL PASSED\n');

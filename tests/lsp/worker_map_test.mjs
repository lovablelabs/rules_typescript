// The tsserver-hook worker's resolution map, hermetically: the worker reads
// only what `bazel run //:refresh_tsconfig` wrote, so a fixture is its input.

import { Worker } from 'node:worker_threads';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const workerPath = process.argv[2];
if (!workerPath || !fs.existsSync(workerPath)) {
  process.stderr.write(`FATAL: worker not found: ${workerPath}\n`);
  process.exit(1);
}

const root = fs.mkdtempSync(path.join(process.env.TEST_TMPDIR || os.tmpdir(), 'wsroot-'));

const write = (rel, contents) => {
  const p = path.join(root, rel);
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, contents);
  return p;
};

// ── The fixture workspace ────────────────────────────────────────────────────

write('MODULE.bazel', 'module(name = "fixture")\n');

// An internal package whose entry point exists only in source.
const libIndex = write('src/lib/index.ts', 'export const a = 1;\n');
// An internal package built into bazel-bin: the .d.ts there must win over the
// .ts source, because that is what the editor should be type-checking against.
const appIndex = write('src/app/index.ts', 'export const b = 2;\n');
const appDts = write('bazel-bin/src/app/index.d.ts', 'export declare const b: number;\n');
// An internal package with no index file at all: nothing to resolve to.
write('src/empty/helpers.ts', 'export const c = 3;\n');

const registrationPackage = 'registration/branch/leaf';
write(`${registrationPackage}/BUILD.bazel`, '');
const registrationEntries = Object.fromEntries(
  ['retired', 'initial', 'published', 'active'].map((name) => [
    name,
    write(`registered/${name}/index.ts`, `export const ${name} = true;\n`),
  ])
);
const fragment = (pkg, owner = registrationPackage) =>
  JSON.stringify({ format: 'tsconfig-fragment-v1', label: `//${owner}:library` }) +
  '\n' +
  JSON.stringify({ package: pkg }) +
  '\n';
const registrationFragment = write(
  `bazel-bin/${registrationPackage}/library.tsconfig-fragment.json`,
  fragment('registered/retired')
);
write('lib/known/BUILD.bazel', '');
const knownDts = write('bazel-bin/lib/known/index.d.ts', 'export declare const known: true;\n');
write('bazel-bin/lib/known/library.tsconfig-fragment.json', fragment('lib/known', 'lib/known'));
const ancestorNoise = write('ancestor-noise.before', 'unrelated activity\n');
const registrationScratch = fs.mkdtempSync(path.join(path.dirname(root), 'registration-'));
const replacementBranch = path.join(registrationScratch, 'replacement');
fs.mkdirSync(path.join(replacementBranch, 'leaf'), { recursive: true });
fs.writeFileSync(
  path.join(replacementBranch, 'leaf/library.tsconfig-fragment.json'),
  fragment('registered/initial')
);

// ── The graph data ───────────────────────────────────────────────────────────

write(
  '.bazel/tsserver-hook-data.json',
  JSON.stringify({ packages: ['src/lib', 'src/app', 'src/empty'] })
);

// ── Run the worker and check the map it sends ───────────────────────────────

let failures = 0;
const pass = (name) => process.stdout.write(`PASS: ${name}\n`);
const fail = (name, detail) => {
  process.stderr.write(`FAIL: ${name}${detail ? ': ' + detail : ''}\n`);
  failures += 1;
};

function expectEntry(map, key, want) {
  if (!(key in map)) {
    fail(`${key} is in the map`, `keys: ${JSON.stringify(Object.keys(map).sort())}`);
    return;
  }
  if (map[key] !== want) {
    fail(`${key} resolves correctly`, `got ${map[key]}, want ${want}`);
    return;
  }
  if (!fs.existsSync(map[key])) {
    fail(`${key} points at a real path`, map[key]);
    return;
  }
  pass(`${key} -> ${map[key]}`);
}

function expectAbsent(map, key, why) {
  if (key in map) {
    fail(`${key} must NOT be in the map (${why})`, `got ${map[key]}`);
    return;
  }
  pass(`${key} is absent (${why})`);
}

function workerBootstrap() {
  const fs = require('fs');
  const path = require('path');
  const { parentPort, workerData } = require('worker_threads');
  const appDirectory = fs.realpathSync(path.join(workerData.workspaceRoot, 'bazel-bin/src/app'));
  const registrationDirectory = fs.realpathSync(
    path.join(workerData.workspaceRoot, 'bazel-bin/registration/branch/leaf')
  );
  const registrationParent = path.dirname(path.dirname(registrationDirectory));
  const watchers = new Map([
    [appDirectory, []],
    [registrationDirectory, []],
    [registrationParent, []],
  ]);
  let replacementParentWatched;
  function observeScheduling(callback) {
    const schedule = global.setTimeout;
    let scheduled = 0;
    global.setTimeout = (...args) => {
      scheduled += 1;
      return schedule(...args);
    };
    try {
      callback();
    } finally {
      global.setTimeout = schedule;
    }
    return scheduled;
  }
  const watch = fs.watch;
  fs.watch = (directory, ...args) => {
    const listener = args.pop();
    const watcher = watch(directory, ...args, (event, filename) => {
      const sibling = path.basename(workerData.ancestorNoise);
      if (
        directory === workerData.workspaceRoot &&
        (filename === sibling || filename === `${sibling}.renamed`)
      ) {
        const scheduled = observeScheduling(() => listener(event, filename));
        post({ type: 'ancestor-sibling-rename', scheduled });
      } else {
        listener(event, filename);
      }
    });
    watchers.get(directory)?.push(watcher);
    if (directory === registrationDirectory && watchers.get(directory).length === 1) {
      replacementParentWatched = watchers.get(registrationParent).length > 0;
      fs.renameSync(path.dirname(directory), path.join(workerData.registrationScratch, 'retired'));
      fs.renameSync(workerData.replacementBranch, path.dirname(directory));
    }
    return watcher;
  };
  const post = parentPort.postMessage.bind(parentPort);
  parentPort.postMessage = (message) =>
    post({
      ...message,
      appWatchCount: watchers.get(appDirectory).length,
      registrationWatchCount: watchers.get(registrationDirectory).length,
      replacementParentWatched,
    });
  require(workerData.workerPath);
  parentPort.on('message', (command) => {
    if (command === 'fragment-activity') {
      const schedule = global.setTimeout;
      let pendingDeadline;
      global.setTimeout = (callback, delay, ...args) => {
        const deadline = performance.now() + delay;
        if (pendingDeadline !== undefined && deadline > pendingDeadline) {
          global.setTimeout = schedule;
          post({ type: 'rebuild-postponed' });
        }
        pendingDeadline = deadline;
        return schedule(() => {
          pendingDeadline = undefined;
          callback(...args);
        }, delay);
      };
      post({ type: command });
    } else if (command === 'active-error' || command === 'retired-error') {
      const scheduled = observeScheduling(() =>
        watchers.get(appDirectory)[0].emit('error', new Error(`fixture ${command}`))
      );
      post({ type: command, scheduled });
    }
  });
}

const worker = new Worker(`(${workerBootstrap.toString()})()`, {
  eval: true,
  workerData: {
    workspaceRoot: root,
    workerPath,
    registrationScratch,
    replacementBranch,
    ancestorNoise,
  },
});
let phase = 'initial resolution map';
let fragmentActivity = null;
let fragmentPublications = 0;
let newDts;

function publishFragment() {
  fs.writeFileSync(`${registrationFragment}.next`, fragment('registered/active'));
  fs.renameSync(`${registrationFragment}.next`, registrationFragment);
  fragmentPublications += 1;
  fragmentActivity = setImmediate(publishFragment);
}

const timeout = setTimeout(() => {
  worker.terminate();
  process.stderr.write(`FAIL: worker did not complete ${phase} within 60s\n`);
  process.exit(1);
}, 60000);

worker.on('error', (err) => {
  clearTimeout(timeout);
  process.stderr.write(`FAIL: worker error: ${err.stack || err.message}\n`);
  process.exit(1);
});

function finish() {
  clearImmediate(fragmentActivity);
  fragmentActivity = null;
  clearTimeout(timeout);
  worker.terminate().then(() => {
    fs.rmSync(registrationScratch, { recursive: true, force: true });
    if (failures > 0) {
      process.stderr.write(`\n${failures} FAILED\n`);
      process.exit(1);
    }
    process.stdout.write('\nALL PASSED\n');
    process.exit(0);
  });
}

// libuv restarts darwin's one FSEvents stream unflushed whenever a rebuild opens or closes a watch.
function afterWatchesSettle(change) {
  setTimeout(change, process.platform === 'darwin' ? 1000 : 0);
}

worker.on('message', (msg) => {
  if (msg.type === 'rebuild-postponed') {
    fail('fragment activity cannot postpone the pending refresh deadline');
    finish();
    return;
  }
  if (msg.type === 'ancestor-sibling-rename') {
    if (phase === 'unrelated ancestor sibling rename') {
      if (msg.scheduled === 0) {
        pass('unrelated ancestor sibling rename cannot schedule map publication');
        phase = 'new sibling package discovery without an existing output change';
        process.stdout.write(`INFO: waiting for ${phase}\n`);
        write('lib/new/BUILD.bazel', '');
        newDts = write('bazel-bin/lib/new/index.d.ts', 'export declare const added: true;\n');
        write('bazel-bin/lib/new/library.tsconfig-fragment.json', fragment('lib/new', 'lib/new'));
      } else {
        phase = 'unexpected ancestor invalidation';
      }
    }
    return;
  }
  if (msg.type === 'fragment-activity' && phase === 'fragment activity setup') {
    phase = 'fragment publication during relevant activity';
    publishFragment();
    return;
  }
  if (msg.type === 'active-error' && phase === 'active watch error') {
    if (msg.scheduled !== 1) {
      fail('active watch error schedules recovery', `scheduled ${msg.scheduled}`);
      finish();
      return;
    }
    pass('active watch error schedules recovery');
    phase = 'reattachment after the active watch error';
    return;
  }
  if (msg.type === 'retired-error' && phase === 'retired watch error') {
    if (msg.scheduled !== 0) {
      fail('retired watch error cannot schedule a rebuild', `scheduled ${msg.scheduled}`);
    } else {
      pass('retired watch error cannot schedule a rebuild');
    }
    phase = 'publication through the replacement watcher';
    fs.unlinkSync(appDts);
    return;
  }
  if (msg.type !== 'resolution-map') {
    fail('expected resolution map', `unexpected message type ${msg.type}`);
    finish();
    return;
  }
  const map = msg.data;
  if (phase === 'initial resolution map') {
    process.stdout.write(`INFO: map = ${JSON.stringify(map, null, 2)}\n`);
    expectEntry(map, 'src/lib', libIndex);
    expectEntry(map, 'src/app', appDts);
    expectAbsent(map, 'src/empty', 'no index.ts/index.d.ts to resolve to');
    expectEntry(map, 'lib/known', knownDts);
    expectEntry(map, 'registered/initial', registrationEntries.initial);
    expectAbsent(map, 'registered/retired', 'registration reads the replacement directory');
    if (msg.replacementParentWatched !== true) {
      fail('ancestor replacement has a subscribed parent', 'no parent watch at replacement');
    } else {
      pass('ancestor replacement has a subscribed parent');
    }
    if (msg.registrationWatchCount !== 1) {
      fail(
        'registration replacement ran after the first leaf attachment',
        msg.registrationWatchCount
      );
    }
    if (msg.appWatchCount !== 1) {
      fail('app directory has one initial watcher', `got ${msg.appWatchCount}`);
    }
    if (failures > 0) {
      finish();
      return;
    }
    phase = 'ancestor replacement during watch registration';
  } else if (phase === 'ancestor replacement during watch registration') {
    expectEntry(map, 'registered/initial', registrationEntries.initial);
    if (msg.registrationWatchCount !== 2) {
      fail(
        'ancestor replacement reattaches the registered leaf',
        `got ${msg.registrationWatchCount} handles`
      );
    } else {
      pass('ancestor replacement reattaches the registered leaf');
    }
    if (failures > 0) {
      finish();
      return;
    }
    phase = 'fragment publication after registration replacement';
    fs.writeFileSync(registrationFragment, fragment('registered/published'));
  } else if (phase === 'fragment publication after registration replacement') {
    expectEntry(map, 'registered/published', registrationEntries.published);
    expectAbsent(map, 'registered/initial', 'later fragment replaces the first published package');
    if (failures > 0) {
      finish();
      return;
    }
    pass('fragment publication reaches the map after registration replacement');
    phase = 'active watch error';
    worker.postMessage('active-error');
  } else if (phase === 'reattachment after the active watch error') {
    expectEntry(map, 'src/app', appDts);
    if (msg.appWatchCount !== 2) {
      fail('active watch error reattaches the directory', `got ${msg.appWatchCount} handles`);
      finish();
      return;
    }
    pass('active watch error reattaches the directory');
    phase = 'retired watch error';
    worker.postMessage('retired-error');
  } else if (phase === 'publication through the replacement watcher') {
    expectEntry(map, 'src/app', appIndex);
    if (msg.appWatchCount !== 2) {
      fail('retired watch error cannot evict the replacement', `got ${msg.appWatchCount} handles`);
    } else {
      pass('replacement watcher publishes after a retired handle error');
    }
    if (failures > 0) {
      finish();
      return;
    }
    phase = 'unrelated ancestor sibling rename';
    fs.renameSync(ancestorNoise, `${ancestorNoise}.renamed`);
  } else if (
    phase === 'unrelated ancestor sibling rename' ||
    phase === 'unexpected ancestor invalidation'
  ) {
    fail('unrelated ancestor sibling rename cannot publish a resolution map');
    finish();
  } else if (phase === 'new sibling package discovery without an existing output change') {
    expectEntry(map, 'lib/new', newDts);
    if (failures > 0) {
      finish();
      return;
    }
    phase = 'new workspace boundary removes the sibling package';
    afterWatchesSettle(() => write('lib/new/MODULE.bazel', 'module(name = "nested")\n'));
  } else if (phase === 'new workspace boundary removes the sibling package') {
    expectAbsent(map, 'lib/new', 'a nested workspace owns the package');
    if (failures > 0) {
      finish();
      return;
    }
    phase = 'removed workspace boundary restores the sibling package';
    afterWatchesSettle(() => fs.unlinkSync(path.join(root, 'lib/new/MODULE.bazel')));
  } else if (phase === 'removed workspace boundary restores the sibling package') {
    expectEntry(map, 'lib/new', newDts);
    if (failures > 0) {
      finish();
      return;
    }
    phase = 'fragment activity setup';
    worker.postMessage('fragment-activity');
  } else if (phase === 'fragment publication during relevant activity') {
    if (!('registered/active' in map)) return;
    expectEntry(map, 'registered/active', registrationEntries.active);
    expectAbsent(map, 'registered/published', 'the fragment changed during relevant activity');
    if (fragmentActivity === null || fragmentPublications === 0) {
      fail('fragment publication completes while relevant updates continue');
    } else {
      pass('fragment publication completes while relevant updates continue');
    }
    finish();
  } else {
    fail('worker published during the expected phase', phase);
    finish();
  }
});

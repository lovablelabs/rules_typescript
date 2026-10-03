import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const bundlePath = process.argv[2];
if (!bundlePath || !fs.existsSync(bundlePath)) {
  process.stderr.write(`FATAL: bundle not found: ${bundlePath}\n`);
  process.exit(1);
}

const { BazelWatcher, ConfigWatcher, bazelPlugin, bazelPathToModuleId } = await import(
  pathToFileURL(bundlePath).href
);

const tmpRoot = process.env.TEST_TMPDIR || os.tmpdir();
let binCounter = 0;
const newBazelBin = () => {
  const dir = path.join(tmpRoot, `bazel-bin-${binCounter++}`);
  fs.mkdirSync(dir, { recursive: true });
  return dir;
};

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function waitUntil(what, predicate, timeoutMs = 10000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await sleep(10);
  }
  throw new Error(`timed out after ${timeoutMs} ms waiting for ${what}`);
}

/** A stand-in for Vite's `server.watcher`, with the surface the plugin uses. */
function fakeViteWatcher() {
  const listeners = new Map();
  return {
    added: [],
    unwatched: [],
    closed: false,
    add(p) {
      this.added.push(p);
      return this;
    },
    unwatch(p) {
      this.unwatched.push(p);
      return this;
    },
    on(event, listener) {
      if (!listeners.has(event)) listeners.set(event, []);
      listeners.get(event).push(listener);
      return this;
    },
    off(event, listener) {
      const fns = listeners.get(event) ?? [];
      const i = fns.indexOf(listener);
      if (i !== -1) fns.splice(i, 1);
      return this;
    },
    close() {
      this.closed = true;
      return Promise.resolve();
    },
    listenerCount(event) {
      return (listeners.get(event) ?? []).length;
    },
    emit(event, filePath) {
      for (const fn of [...(listeners.get(event) ?? [])]) fn(filePath);
    },
  };
}

function fakeDevServer(watcher) {
  const warnings = [];
  const infos = [];
  const sent = [];
  const invalidated = [];
  const logger = {
    info(message) {
      infos.push(message);
    },
    warn(message) {
      warnings.push(message);
    },
    error(message) {
      warnings.push(message);
    },
  };
  return {
    warnings,
    infos,
    sent,
    invalidated,
    restarts: 0,
    logger,
    watcher,
    restart() {
      this.restarts++;
      return Promise.resolve();
    },
    config: { root: tmpRoot, cacheDir: path.join(tmpRoot, '.vite'), logger },
    environments: {
      client: {
        config: { root: tmpRoot, resolve: { extensions: [] } },
        async transformRequest() { return null; },
      },
    },
    moduleGraph: {
      getModulesByFile: (file) => file.startsWith(tmpRoot + path.sep)
        ? new Set([{ file, url: "/@fs" + file }])
        : undefined,
      invalidateModule: (mod) => invalidated.push(mod.file),
      invalidateAll: () => invalidated.push('*'),
    },
    reloadModule(mod) {
      invalidated.push(mod.file);
      sent.push({ type: 'update', updates: [{ path: mod.url }] });
      return Promise.resolve();
    },
    ws: { send: (payload) => sent.push(payload) },
  };
}

/**
 * Runs `configureServer` the way Vite does: a function it returns is a post hook
 * that Vite invokes once the internal middlewares are in place, not a teardown
 * handle it stores for shutdown. A plugin that returns its teardown from here
 * detaches its own watchers before the first request.
 */
async function installPlugin(plugin, server) {
  const post = await plugin.configureServer(server);
  if (typeof post === 'function') post();
  return post;
}

const tests = [];
const test = (name, fn) => tests.push([name, fn]);

class Skipped extends Error {}
const skip = (why) => {
  throw new Skipped(why);
};

// Some sandboxed filesystems accept fs.watch without delivering events.
let fsWatchDelivers = null;
async function fsWatchDeliversEvents() {
  if (fsWatchDelivers !== null) return fsWatchDelivers;
  const bin = newBazelBin();
  let fired = false;
  const probe = fs.watch(bin, { persistent: true }, () => {
    fired = true;
  });
  try {
    fs.writeFileSync(path.join(bin, 'probe.js'), '1');
    const deadline = Date.now() + 5000;
    while (!fired && Date.now() < deadline) await sleep(20);
  } finally {
    probe.close();
  }
  fsWatchDelivers = fired;
  return fired;
}

const NO_FS_EVENTS = 'fs.watch delivered no events in this environment';

const ARM_SENTINEL = '__arm__.js';
const ARM_DEADLINE_MS = 30000;

// Darwin FSEvents can drop writes before its run loop arms the stream.
// Observe a real sentinel event before testing subsequent filesystem changes.
async function startArmed(start, bin, batches, debounceMs) {
  const result = await start();
  const sentinel = path.join(bin, ARM_SENTINEL);
  const deadline = Date.now() + ARM_DEADLINE_MS;
  for (let n = 0; batches.length === 0; n += 1) {
    if (Date.now() >= deadline) {
      throw new Error(
        `fs.watch never armed for ${bin}: ${n} sentinel writes in ` +
          `${ARM_DEADLINE_MS} ms produced no batch`,
      );
    }
    fs.writeFileSync(sentinel, String(n));
    // A delivered event only becomes a batch after the trailing debounce, so
    // an attempt has to outlast one or every attempt reads as a loss.
    await waitUntil('a sentinel batch', () => batches.length > 0, debounceMs + 500).catch(
      () => {},
    );
  }
  for (let seen = -1; seen !== batches.length; ) {
    seen = batches.length;
    await sleep(Math.max(debounceMs * 2, 200));
  }
  batches.length = 0;
  return result;
}

// ── The fs.watch fallback: real files, real events ───────────────────────────

test('native watches follow replaced child directories and release retired handles', async () => {
  if (!(await fsWatchDeliversEvents())) skip(NO_FS_EVENTS);
  const realBin = newBazelBin();
  const bin = realBin + '-link';
  fs.symlinkSync(realBin, bin, 'dir');
  const batches = [];
  const watcher = new BazelWatcher({
    bazelBin: bin,
    debounceMs: 20,
    declaredFiles: { app: { path: path.join(realBin, 'app.js'), context: 'source' } },
    onRebuild: (changed) => batches.push([...changed]),
  });

  const nativeWatch = fs.watch;
  const handles = new Map();
  fs.watch = (file, ...args) => {
    assert(![...handles.values()].includes(file), `duplicate native handle for ${file}`);
    const handle = nativeWatch(file, ...args);
    handles.set(handle, file);
    const close = handle.close.bind(handle);
    handle.close = () => {
      handles.delete(handle);
      close();
    };
    return handle;
  };
  try {
    await startArmed(() => watcher.start(), bin, batches, 20);
    const identities = [...handles.values()].map((file) => {
      const stat = fs.statSync(file);
      return `${stat.dev}:${stat.ino}`;
    });
    assert.equal(new Set(identities).size, identities.length, 'canonical root gained a duplicate handle');
    fs.writeFileSync(path.join(bin, 'app.js'), 'export const a = 1;\n');
    await waitUntil('the first rebuild batch', () => batches.length > 0);
    assert.deepEqual(batches[0], [path.join(bin, 'app.js')]);

    const staged = path.join(tmpRoot, path.basename(bin) + '-staged');
    const compiled = path.join(bin, 'compiled');
    const nested = path.join(compiled, 'nested');
    const entry = path.join(nested, 'entry.js');
    fs.mkdirSync(path.join(staged, 'nested'), { recursive: true });
    fs.writeFileSync(path.join(staged, 'nested', 'entry.js'), 'export const value = 1;');
    fs.symlinkSync('entry.js', path.join(staged, 'nested', 'alias.js'));
    batches.length = 0;
    fs.renameSync(staged, compiled);
    await waitUntil('the populated directory import', () =>
      batches.some((batch) => batch.includes(entry)),
    );
    const retired = [...handles].filter(([, file]) =>
      file === compiled || file.startsWith(compiled + path.sep),
    );
    assert.equal(retired.length, 3);

    fs.renameSync(compiled, staged);
    fs.mkdirSync(nested, { recursive: true });
    fs.writeFileSync(entry, 'export const value = 2;');
    batches.length = 0;
    await waitUntil('the replaced directory coordinate', () =>
      batches.some((batch) => batch.includes(compiled)),
    );
    for (const [handle] of retired) {
      assert(!handles.has(handle), 'the old directory handle was retained');
    }
    assert.equal([...handles.values()].filter((file) => file === nested).length, 1);

    batches.length = 0;
    fs.writeFileSync(entry, 'export const value = 3;');
    await waitUntil('the replacement directory leaf update', () =>
      batches.some((batch) => batch.includes(entry)),
    );
    batches.length = 0;
    fs.rmSync(compiled, { recursive: true });
    await waitUntil('the removed directory coordinate', () =>
      batches.some((batch) => batch.includes(compiled)),
    );
    assert(![...handles.values()].some((file) =>
      file === compiled || file.startsWith(compiled + path.sep),
    ));
  } finally {
    fs.unlinkSync(bin);
    await watcher.stop();
    fs.watch = nativeWatch;
  }
  assert.equal(handles.size, 0, 'stop left native handles open');
});

test('fs.watch fallback coalesces a rebuild burst and drops non-.js outputs', async () => {
  if (!(await fsWatchDeliversEvents())) skip(NO_FS_EVENTS);
  const bin = newBazelBin();
  const batches = [];
  // The debounce is trailing, so a burst coalesces while the gaps between
  // events stay under it -- and on macOS the gaps are FSEvents flush intervals,
  // which 200ms did not cover. The window is incidental to what is asserted.
  const debounceMs = 1000;
  const watcher = new BazelWatcher({
    bazelBin: bin,
    debounceMs: debounceMs,
    onRebuild: (changed) => batches.push([...changed].sort()),
  });

  await startArmed(() => watcher.start(), bin, batches, debounceMs);
  try {
    fs.mkdirSync(path.join(bin, 'app'), { recursive: true });
    fs.writeFileSync(path.join(bin, 'a.js'), '1');
    fs.writeFileSync(path.join(bin, 'app', 'b.js'), '2');
    fs.writeFileSync(path.join(bin, 'a.js.map'), '{}');
    fs.writeFileSync(path.join(bin, 'styles.css'), 'a{}');

    await waitUntil('the coalesced batch', () => batches.length > 0);
    await sleep(debounceMs * 1.5);

    // What the real fs.watch path can promise: the burst arrives as one batch,
    // and nothing that is not a .js under bazel-bin is in it. Which of the two
    // .js files arrive is FSEvents' business -- it coalesces per directory, so
    // writing a.js, a.js.map and styles.css into one directory can surface a
    // single event, and if it names one of the two filtered files the a.js
    // event is simply never delivered. The exact set is asserted in the
    // injected-source test above, where delivery is deterministic.
    assert.equal(batches.length, 1, `expected one batch, got ${batches.length}`);
    const allowed = [path.join(bin, 'a.js'), path.join(bin, 'app', 'b.js')];
    assert.ok(batches[0].length > 0, 'the batch is empty');
    for (const entry of batches[0]) {
      assert.ok(
        allowed.includes(entry),
        `${entry} is in the batch and is not one of the two .js files written`,
      );
    }
  } finally {
    await watcher.stop();
  }
});

test('fs.watch fallback stops reporting after stop()', async () => {
  if (!(await fsWatchDeliversEvents())) skip(NO_FS_EVENTS);
  const bin = newBazelBin();
  const batches = [];
  const watcher = new BazelWatcher({
    bazelBin: bin,
    debounceMs: 20,
    onRebuild: (changed) => batches.push([...changed]),
  });

  // Armed first: an unarmed watcher reports nothing either, and would pass
  // this without stop() having done anything.
  await startArmed(() => watcher.start(), bin, batches, 20);
  await watcher.stop();
  fs.writeFileSync(path.join(bin, 'late.js'), '1');
  await sleep(300);
  assert.deepEqual(batches, []);
});

// ── Vite's own watcher: the path the plugin takes ────────────────────────────

test("an injected source is added to, filtered, and left open by stop()", async () => {
  const bin = newBazelBin();
  const source = fakeViteWatcher();
  const batches = [];
  const watcher = new BazelWatcher({
    bazelBin: bin,
    debounceMs: 20,
    source,
    onRebuild: (changed) => batches.push([...changed]),
  });

  await watcher.start();
  assert.deepEqual(source.added, [bin], 'bazel-bin must be added to the watcher');

  source.emit('change', path.join(bin, 'app.js'));
  source.emit('add', path.join(bin, 'nested', 'x.js'));
  source.emit('change', path.join(bin, 'styles.css'));
  source.emit('change', path.join(tmpRoot, 'outside', 'other.js'));

  await waitUntil('the rebuild batch', () => batches.length > 0);
  assert.deepEqual(batches[0].sort(), [
    path.join(bin, 'app.js'),
    path.join(bin, 'nested', 'x.js'),
  ]);

  await watcher.stop();
  assert.equal(source.listenerCount('change'), 0, 'listeners must be detached');
  assert.deepEqual(source.unwatched, [bin]);
  assert.equal(source.closed, false, "Vite's watcher must not be closed");
});

// ── The failure is never silent ──────────────────────────────────────────────

test('start() rejects missing trees and closes partial native watches', async () => {
  const watcher = new BazelWatcher({
    bazelBin: path.join(tmpRoot, 'no-such-bazel-bin'),
    onRebuild: () => {},
  });
  await assert.rejects(() => watcher.start(), /bazel-bin does not exist/);

  const bin = newBazelBin();
  fs.mkdirSync(path.join(bin, 'child'));
  const nativeWatch = fs.watch;
  const opened = [];
  fs.watch = (file, ...args) => {
    if (file !== bin) throw new Error('child watch cannot start');
    const handle = nativeWatch(file, ...args);
    const record = { handle, closed: false };
    opened.push(record);
    const close = handle.close.bind(handle);
    handle.close = () => {
      record.closed = true;
      close();
    };
    return handle;
  };
  try {
    const partial = new BazelWatcher({ bazelBin: bin, onRebuild() {} });
    await assert.rejects(() => partial.start(), /child watch cannot start/);
    assert.equal(opened.length, 1);
    assert(opened[0].closed, 'failed startup left the root watch open');
    await partial.stop();
  } finally {
    fs.watch = nativeWatch;
    for (const { handle } of opened) handle.close();
  }
});

// ── The plugin end to end ────────────────────────────────────────────────────

test('nested manual configs resolve tool paths from the selected workspace', () => {
  const workspaceRoot = fs.mkdtempSync(path.join(tmpRoot, 'nested-config-'));
  const appRoot = path.join(workspaceRoot, 'app');
  fs.mkdirSync(appRoot);
  const importer = path.join(appRoot, 'entry.ts');
  fs.writeFileSync(importer, 'import "./generated.js";');
  for (const options of [
    { workspaceRoot, nodeModules: 'node_modules' },
    { workspaceRoot, bazelBin: 'out', nodeModules: 'deps' },
    { nodeModules: 'node_modules' },
  ]) {
    const base = options.workspaceRoot ?? appRoot;
    const bin = path.join(base, options.bazelBin ?? 'bazel-bin');
    const generated = path.join(bin, path.relative(base, appRoot), 'generated.js');
    fs.mkdirSync(path.dirname(generated), { recursive: true });
    fs.writeFileSync(generated, 'export const generated = true;');
    const plugin = bazelPlugin(options);
    const config = plugin.config({ root: appRoot }, { command: 'serve', mode: 'development' });
    assert(config.server.fs.allow.includes(bin));
    assert(config.server.fs.allow.includes(path.join(base, options.nodeModules)));
    plugin.configResolved({ root: appRoot, logger: { info() {} } });
    assert.equal(plugin.resolveId.handler('./generated.js', importer), generated);
  }
});

test('Vite cache and declared output replacements preserve HMR across producer configurations', async () => {
  const realBin = newBazelBin();
  const bin = realBin + '-link';
  fs.symlinkSync(realBin, bin, 'dir');
  const source = fakeViteWatcher();
  const server = fakeDevServer(source);
  server.config.cacheDir = path.join(realBin, 'optimizer-output');
  const outputDir = path.join(bin, 'optimizer-output-sources');
  const cacheTemp = path.join(server.config.cacheDir, 'pending');
  fs.mkdirSync(cacheTemp, { recursive: true });
  fs.writeFileSync(path.join(cacheTemp, 'dependency.js'), 'export const dependency = 1;');
  const producerTarget = fs.mkdtempSync(path.join(tmpRoot, 'producer-config-'));
  const producer = producerTarget + '-link';
  fs.symlinkSync(producerTarget, producer, 'dir');
  const inputs = [
    ['page.ts', 'source', 'export const page = 1;'],
    ['payload.json', 'source', '{"value":1}'],
    ['styles.css', 'asset', 'body { color: blue; }'],
    ['cross-config.json', 'source', '{"value":1}', producer],
  ];
  fs.mkdirSync(outputDir);
  const declaredFiles = {};
  for (const [name, context, content, directory = outputDir] of inputs) {
    const file = path.join(directory, name);
    fs.writeFileSync(file, content);
    declaredFiles[`app/${name}`] = { path: file, context };
  }
  const generatedTree = path.join(producer, 'generated-tree');
  const treeTargets = ['initial', 'replacement'].map((name) => {
    const directory = fs.mkdtempSync(path.join(tmpRoot, `generated-tree-${name}-`));
    fs.mkdirSync(path.join(directory, 'messages'));
    fs.writeFileSync(path.join(directory, 'messages/greeting.js'), 'export const greeting = 1;');
    return directory;
  });
  fs.symlinkSync(treeTargets[0], generatedTree, 'dir');
  const nested = path.join(generatedTree, 'messages/greeting.js');
  const loaded = new Set([
    path.join(bin, ARM_SENTINEL),
    ...Object.values(declaredFiles).map((file) => file.path),
    nested,
  ]);
  declaredFiles['app/generated-tree'] = { path: generatedTree, context: 'source', directory: true };
  server.moduleGraph.getModulesByFile = (file) => loaded.has(file)
    ? new Set([{ file, url: '/@fs' + file }])
    : undefined;
  const plugin = bazelPlugin({ bazelBin: bin, hmrDebounceMs: 20, mode: 'serve', declaredFiles });

  plugin.configResolved(server.config);
  try {
    const post = await startArmed(() => installPlugin(plugin, server), bin, server.sent, 20);
    assert.equal(post, undefined, 'configureServer must return nothing: Vite calls what it returns');
    assert.deepEqual(source.added, [], 'the plugin does not depend on the server watcher');

    fs.renameSync(cacheTemp, path.join(server.config.cacheDir, 'ready'));
    for (const [name, _context, content, directory = outputDir] of inputs) {
      const changed = path.join(directory, name);
      fs.writeFileSync(changed, content + '\n');
      await waitUntil('an HMR update for ' + name, () =>
        server.sent.some((event) =>
          event.type === 'update' && event.updates.some((update) => update.path === '/@fs' + changed),
        ),
      );
      assert(server.invalidated.includes(changed));
      assert(!server.sent.some((event) => event.type === 'full-reload'));
    }
    const css = path.join(outputDir, 'styles.css');
    const replacement = path.join(tmpRoot, path.basename(bin) + '-replacement.css');
    for (const revision of ['first', 'second']) {
      server.sent.length = 0;
      server.invalidated.length = 0;
      if (revision === 'second') fs.rmSync(server.config.cacheDir, { recursive: true });
      fs.writeFileSync(replacement, `body { --revision: ${revision}; }\n`);
      fs.renameSync(replacement, css);
      await waitUntil('an HMR update for the ' + revision + ' atomic CSS replacement', () =>
        server.sent.some((event) =>
          event.type === 'update' && event.updates.some((update) => update.path === '/@fs' + css),
        ),
      );
      assert(server.invalidated.includes(css));
      assert(!server.sent.some((event) => event.type === 'full-reload'));
    }
    const scalar = path.join(producer, 'cross-config.json');
    for (const revision of [2, 3]) {
      server.sent.length = 0;
      server.invalidated.length = 0;
      fs.writeFileSync(scalar + '.next', JSON.stringify({ value: revision }));
      fs.renameSync(scalar + '.next', scalar);
      await waitUntil('an HMR update for atomic cross-config scalar replacement ' + revision, () =>
        server.sent.some((event) =>
          event.type === 'update' && event.updates.some((update) => update.path === '/@fs' + scalar),
        ),
      );
      assert(server.invalidated.includes(scalar));
      assert(!server.sent.some((event) => event.type === 'full-reload'));
    }
    for (const [index, target] of treeTargets.entries()) {
      if (index > 0) {
        server.sent.length = 0;
        fs.symlinkSync(target, generatedTree + '.next', 'dir');
        fs.renameSync(generatedTree + '.next', generatedTree);
        await waitUntil('the cross-config declared tree replacement reload', () =>
          server.sent.some((event) => event.type === 'full-reload'),
        );
      }
      server.sent.length = 0;
      server.invalidated.length = 0;
      fs.writeFileSync(path.join(target, 'messages/greeting.js'), `export const greeting = ${index + 2};`);
      await waitUntil('an HMR update for cross-config nested tree member revision ' + index, () =>
        server.sent.some((event) =>
          event.type === 'update' && event.updates.some((update) => update.path === '/@fs' + nested),
        ),
      );
      assert(server.invalidated.includes(nested));
      assert(!server.sent.some((event) => event.type === 'full-reload'));
    }
    server.sent.length = 0;
    server.invalidated.length = 0;
    fs.renameSync(outputDir, path.join(tmpRoot, path.basename(bin) + '-removed'));
    await waitUntil('the removed module directory reload', () =>
      server.sent.some((event) => event.type === 'full-reload'),
    );
    assert(server.invalidated.includes('*'));
    assert.deepEqual(server.warnings, []);
  } finally {
    plugin.closeBundle();
  }
  assert.equal(source.listenerCount('change'), 0);
});

test('unloaded authored source edits stay out of the Bazel reload path', async () => {
  if (!(await fsWatchDeliversEvents())) skip(NO_FS_EVENTS);
  const bin = newBazelBin();
  const workspaceRoot = fs.mkdtempSync(path.join(tmpRoot, 'authored-watch-'));
  const authoredDirectory = path.join(workspaceRoot, 'authored');
  const sharedDirectory = path.join(workspaceRoot, 'shared');
  fs.mkdirSync(authoredDirectory);
  fs.mkdirSync(sharedDirectory);
  const authored = [
    path.join(authoredDirectory, 'unloaded.ts'),
    path.join(sharedDirectory, 'unloaded.ts'),
  ];
  const generated = path.join(sharedDirectory, 'generated.json');
  for (const file of authored) fs.writeFileSync(file, 'export const value = 1;');
  fs.writeFileSync(generated, '{"value":1}');
  const declaredFiles = Object.fromEntries(authored.map((file) => [
    path.relative(workspaceRoot, file), { path: file, context: 'source', isSource: true },
  ]));
  declaredFiles['app/generated.json'] = { path: generated, context: 'source', isSource: false };
  const server = fakeDevServer(fakeViteWatcher());
  const loaded = new Set([path.join(bin, ARM_SENTINEL), generated]);
  server.moduleGraph.getModulesByFile = (file) => loaded.has(file)
    ? new Set([{ file, url: '/@fs' + file }])
    : undefined;
  const plugin = bazelPlugin({
    bazelBin: bin, workspaceRoot, mode: 'serve', declaredFiles, hmrDebounceMs: 20,
  });
  const nativeWatch = fs.watch;
  const subscribed = new Set();
  const observed = new Set();
  fs.watch = (file, options, listener) => {
    subscribed.add(file);
    return nativeWatch(file, options, (event, filename) => {
      if (filename !== null) observed.add(path.resolve(file, filename.toString()));
      listener(event, filename);
    });
  };
  plugin.configResolved(server.config);
  try {
    await startArmed(() => installPlugin(plugin, server), bin, server.sent, 20);
    server.invalidated.length = 0;
    for (const file of authored) fs.writeFileSync(file, 'export const value = 2;');
    await waitUntil('the authored edit beside a generated output', () => observed.has(authored[1]));
    fs.writeFileSync(generated, '{"value":2}');
    await waitUntil('the generated update or an unwanted authored reload', () =>
      server.sent.some((event) => event.type === 'full-reload' ||
        (event.type === 'update' && event.updates.some((update) => update.path === '/@fs' + generated))),
    );
    assert(!server.sent.some((event) => event.type === 'full-reload'),
      'an unloaded authored source edit reached the Bazel reload path');
    assert(!server.invalidated.includes('*'), 'an authored edit invalidated the complete module graph');
    assert(server.invalidated.includes(generated), 'the generated output lost its HMR notification');
    assert(!subscribed.has(authoredDirectory), 'Bazel subscribed to an authored-only directory');
    assert.deepEqual(server.watcher.added, []);
  } finally {
    plugin.closeBundle();
    fs.watch = nativeWatch;
  }
});

test('a watcher that cannot start warns, and throws when hmr is required', async () => {
  const missing = path.join(tmpRoot, 'never-built');
  const server = fakeDevServer(fakeViteWatcher());
  const plugin = bazelPlugin({ bazelBin: missing });

  plugin.configResolved(server.config);
  await installPlugin(plugin, server);

  assert.equal(server.warnings.length, 1, 'the failure must be reported');
  assert.match(server.warnings[0], /no HMR/);
  assert.match(server.warnings[0], /bazel-bin does not exist/);

  const strict = bazelPlugin({ bazelBin: missing, hmr: true });
  const strictServer = fakeDevServer(fakeViteWatcher());
  strict.configResolved(strictServer.config);
  await assert.rejects(() => strict.configureServer(strictServer), /no HMR/);
});

// ── Restart or keep: what a rebuild means ────────────────────────────────────

test('a changed config input restarts the server, a codegen rebuild does not', async () => {
  const bin = newBazelBin();
  const configPath = path.join(bin, 'app', 'dev', 'vite.config.mjs');
  fs.mkdirSync(path.dirname(configPath), { recursive: true });
  fs.writeFileSync(configPath, '// v1\n');
  const codegen = path.join(bin, 'app', 'routes.gen.js');
  fs.writeFileSync(codegen, 'export const routes = [1];\n');

  const source = fakeViteWatcher();
  const server = fakeDevServer(source);
  const plugin = bazelPlugin({
    bazelBin: bin,
    hmrDebounceMs: 20,
    configInputs: [
      {
        label: 'the generated vite config',
        path: configPath,
        digest: 'content',
        remedy: 'restart',
      },
    ],
  });

  plugin.configResolved(server.config);
  await startArmed(() => installPlugin(plugin, server), bin, server.sent, 20);
  assert.deepEqual(source.added, []);

  // A rebuild that only rewrote generated code: the running server is still
  // configured for the graph it has.
  fs.writeFileSync(codegen, 'export const routes = [1, 2];\n');
  await waitUntil('the HMR update for the codegen rebuild', () => server.sent.length > 0);
  await sleep(200);
  assert.equal(server.restarts, 0, 'a codegen-only rebuild must not restart Vite');

  // A rebuild that rewrote the config: the graph it describes is gone.
  fs.writeFileSync(configPath + '.tmp', '// v2 — new aliases\n');
  fs.renameSync(configPath + '.tmp', configPath);
  await waitUntil('the restart', () => server.restarts > 0);
  assert.match(server.infos.join('\n'), /restarting: the generated vite config/);

  plugin.closeBundle();
  const restarts = server.restarts;
  fs.writeFileSync(configPath, '// after stop\n');
  await sleep(100);
  assert.equal(server.restarts, restarts);
});

test('config watcher closes earlier directory watches when startup fails', async () => {
  const bin = newBazelBin();
  const config = path.join(bin, 'config.mjs');
  fs.writeFileSync(config, 'before');
  let changes = 0;
  const watcher = new ConfigWatcher({
    inputs: [config, path.join(bin, 'missing', 'config.mjs')].map((file) => ({
      label: file, path: file, digest: 'content', remedy: 'restart',
    })),
    onStale: () => changes++,
  });
  await assert.rejects(() => watcher.start(), /ENOENT/);
  fs.writeFileSync(config, 'after');
  await sleep(100);
  assert.equal(changes, 0);
});

test('a config input a restart cannot fix says so', async () => {
  const bin = newBazelBin();
  const npmTree = path.join(bin, 'node_modules_stamp');
  fs.writeFileSync(npmTree, '{"version":"6.0.0"}\n');

  const source = fakeViteWatcher();
  const server = fakeDevServer(source);
  const plugin = bazelPlugin({
    bazelBin: bin,
    hmrDebounceMs: 20,
    configInputs: [
      { label: 'vite in the Bazel npm tree', path: npmTree, digest: 'content', remedy: 'manual' },
    ],
  });

  plugin.configResolved(server.config);
  // macOS FSEvents drops writes made before its stream is live.
  await startArmed(() => installPlugin(plugin, server), bin, server.sent, 20);

  fs.writeFileSync(npmTree, '{"version":"7.0.0"}\n');
  await waitUntil('the restart', () => server.restarts > 0);
  assert.match(server.warnings.join('\n'), /re-run `bazel run` on this target/);

  plugin.closeBundle();
});

test('hmr: false starts no watcher at all', async () => {
  const bin = newBazelBin();
  const source = fakeViteWatcher();
  const server = fakeDevServer(source);
  const plugin = bazelPlugin({ bazelBin: bin, hmr: false });

  plugin.configResolved(server.config);
  const post = await installPlugin(plugin, server);

  assert.equal(post, undefined);
  assert.deepEqual(source.added, []);
  assert.deepEqual(server.warnings, []);
});

test('native watches retain nested declared tree updates across symlink replacement', async () => {
  if (!(await fsWatchDeliversEvents())) skip(NO_FS_EVENTS);
  const realBin = newBazelBin();
  const bin = realBin + '-link';
  fs.symlinkSync(realBin, bin, 'dir');
  const external = fs.mkdtempSync(path.join(tmpRoot, 'watch-target-'));
  const target = path.join(external, 'value.js');
  const nextTarget = path.join(external, 'next.js');
  const targetDirectory = path.join(external, 'directory');
  fs.writeFileSync(target, '1');
  fs.writeFileSync(nextTarget, '2');
  const replacementDirectory = path.join(external, 'replacement-directory');
  const privateDirectory = path.join(external, 'private-directory');
  fs.mkdirSync(path.join(privateDirectory, 'nested'), { recursive: true });
  for (const directory of [targetDirectory, replacementDirectory]) {
    fs.mkdirSync(path.join(directory, 'messages'), { recursive: true });
    fs.writeFileSync(path.join(directory, 'messages/greeting.js'), 'export const greeting = 1;');
    fs.symlinkSync(directory, path.join(directory, 'cycle'), 'dir');
    fs.symlinkSync(privateDirectory, path.join(directory, 'private'), 'dir');
  }
  const linkedFile = path.join(bin, 'linked.js');
  const linkedDirectory = path.join(bin, 'linked-directory');
  const cycleA = path.join(bin, 'cycle-a.js');
  const cycleB = path.join(bin, 'cycle-b.js');
  const lateCycle = path.join(bin, 'late-cycle');
  const healthy = path.join(bin, 'healthy.js');
  fs.symlinkSync(target, linkedFile);
  fs.symlinkSync(targetDirectory, linkedDirectory, 'dir');
  fs.symlinkSync('cycle-b.js', cycleA);
  fs.symlinkSync('cycle-a.js', cycleB);
  assert.throws(() => fs.statSync(cycleA), { code: 'ELOOP' });
  fs.symlinkSync(path.join(external, 'missing'), path.join(bin, 'dangling'));
  fs.symlinkSync(path.join(target, 'child'), path.join(bin, 'not-directory'));
  fs.writeFileSync(healthy, '1');
  const batches = [];
  const watcher = new BazelWatcher({
    bazelBin: bin,
    debounceMs: 20,
    declaredFiles: { tree: { path: linkedDirectory, context: 'source', directory: true } },
    isDeclaredFile: (file) => file === linkedDirectory || file.startsWith(linkedDirectory + path.sep),
    onRebuild: (changed) => batches.push([...changed]),
  });
  const nativeReadDirectory = fs.readdirSync;
  const scanned = [];
  fs.readdirSync = (directory, ...args) => {
    scanned.push(directory);
    return nativeReadDirectory(directory, ...args);
  };
  try {
    await startArmed(() => watcher.start(), bin, batches, 20);
    const nested = path.join(linkedDirectory, 'messages/greeting.js');
    fs.writeFileSync(path.join(targetDirectory, 'messages/greeting.js'), 'export const greeting = 2;');
    await waitUntil('the existing nested declared tree member edit', () =>
      batches.some((batch) => batch.includes(nested)),
    );
    batches.length = 0;
    fs.writeFileSync(target, '3');
    await waitUntil('the unchanged link target update', () =>
      batches.some((batch) => batch.includes(linkedFile)),
    );
    batches.length = 0;
    fs.unlinkSync(linkedFile);
    fs.symlinkSync('late-cycle', linkedFile);
    fs.symlinkSync('linked.js', lateCycle);
    assert.throws(() => fs.statSync(linkedFile), { code: 'ELOOP' });
    await waitUntil('the link becoming cyclic', () =>
      batches.some((batch) => batch.includes(linkedFile)),
    );
    batches.length = 0;
    fs.writeFileSync(healthy, '2');
    await waitUntil('a valid file update beside the cyclic link', () =>
      batches.some((batch) => batch.includes(healthy)),
    );
    batches.length = 0;
    fs.unlinkSync(linkedFile);
    fs.symlinkSync(nextTarget, linkedFile);
    await waitUntil('the replaced link update', () =>
      batches.some((batch) => batch.includes(linkedFile)),
    );
    batches.length = 0;
    fs.unlinkSync(cycleA);
    fs.symlinkSync(target, cycleA);
    await waitUntil('the startup cycle replacement', () =>
      batches.some((batch) => batch.includes(cycleA)),
    );
    batches.length = 0;
    fs.writeFileSync(target, '5');
    await waitUntil('the startup cycle replacement target update', () =>
      batches.some((batch) => batch.includes(cycleA)),
    );
    batches.length = 0;
    fs.writeFileSync(nextTarget, '4');
    await waitUntil('the replacement link target update', () =>
      batches.some((batch) => batch.includes(linkedFile)),
    );
    batches.length = 0;
    fs.writeFileSync(path.join(targetDirectory, 'new.json'), '{}');
    await waitUntil('the declared linked directory member update', () =>
      batches.some((batch) => batch.includes(path.join(linkedDirectory, 'new.json'))),
    );
    batches.length = 0;
    const replacementLink = linkedDirectory + '.next';
    fs.symlinkSync(replacementDirectory, replacementLink, 'dir');
    fs.renameSync(replacementLink, linkedDirectory);
    await waitUntil('the declared tree symlink replacement', () =>
      batches.some((batch) => batch.includes(linkedDirectory)),
    );
    batches.length = 0;
    fs.writeFileSync(path.join(replacementDirectory, 'messages/greeting.js'), 'export const greeting = 3;');
    await waitUntil('the replacement nested declared tree member edit', () =>
      batches.some((batch) => batch.includes(nested)),
    );
    for (const name of ['cycle', 'private']) {
      const excluded = path.join(linkedDirectory, name);
      assert(!scanned.some((directory) => directory === excluded || directory.startsWith(excluded + path.sep)),
        `watch traversal escaped through ${name}`);
    }
  } finally {
    await watcher.stop();
    fs.readdirSync = nativeReadDirectory;
  }
});

let failed = 0;
let skipped = 0;
for (const [name, fn] of tests) {
  try {
    await fn();
    process.stdout.write(`PASS: ${name}\n`);
  } catch (err) {
    if (err instanceof Skipped) {
      skipped++;
      process.stdout.write(`SKIP: ${name} -- ${err.message}\n`);
      continue;
    }
    failed++;
    process.stdout.write(`FAIL: ${name}\n${err && err.stack ? err.stack : err}\n`);
  }
}

const passed = tests.length - failed - skipped;
process.stdout.write(`\n${passed}/${tests.length} passed${skipped ? `, ${skipped} skipped` : ''}\n`);
process.exit(failed === 0 ? 0 : 1);

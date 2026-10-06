// The Bazel resolution map, shared by tsserver-plugin.js and tsserver-hook.js.
// Node builtins only: nothing installs dependencies under .bazel.

'use strict';

const { Worker } = require('worker_threads');
const fs = require('fs');
const path = require('path');

function debug(msg) {
  if (process.env.TSSERVER_HOOK_DEBUG) {
    process.stderr.write(`[tsserver-hook] ${msg}\n`);
  }
}

function findWorkspaceRoot(startDir) {
  let dir = startDir;
  for (;;) {
    if (fs.existsSync(path.join(dir, 'MODULE.bazel'))) return dir;
    const parent = path.dirname(dir);
    // No MODULE.bazel anywhere above: hand back where we started and let the
    // worker find nothing, rather than failing to load.
    if (parent === dir) return startDir;
    dir = parent;
  }
}

function extensionOf(fileName) {
  if (/\.d\.[mc]?ts$/.test(fileName)) return '.d.ts';
  if (fileName.endsWith('.tsx')) return '.tsx';
  if (fileName.endsWith('.mts')) return '.mts';
  if (fileName.endsWith('.cts')) return '.cts';
  return '.ts';
}

/**
 * Build a TypeScript ResolvedModuleWithFailedLookupLocations from a file path.
 *
 * @param {string} resolvedFileName - Absolute path to the resolved file.
 * @returns {{ resolvedModule: object }}
 */
function buildResolvedModule(resolvedFileName) {
  return {
    resolvedModule: {
      resolvedFileName,
      isExternalLibraryImport: false,
      extension: extensionOf(resolvedFileName),
    },
  };
}

/**
 * Start the worker for a workspace and answer module names out of its map.
 *
 * `onUpdate` runs after every map replacement, which is a caller's chance to
 * invalidate whatever it resolved from the map before.
 */
function createResolutionSource({ workspaceRoot, workerPath, onUpdate }) {
  // Key: a first-party package path ("src/utils"); value: the absolute path
  // of its .d.ts / .ts.
  const cache = new Map();
  let ready = false;
  let worker;

  // TSSERVER_HOOK_PRELOAD_MAP populates the cache synchronously, so a test can
  // assert against a fixed map without racing the worker.
  if (process.env.TSSERVER_HOOK_PRELOAD_MAP) {
    try {
      for (const [key, value] of Object.entries(
        JSON.parse(process.env.TSSERVER_HOOK_PRELOAD_MAP)
      )) {
        cache.set(key, value);
      }
      ready = true;
      debug(`preloaded ${cache.size} entries from TSSERVER_HOOK_PRELOAD_MAP`);
    } catch (e) {
      debug(`failed to parse TSSERVER_HOOK_PRELOAD_MAP: ${e.message}`);
    }
  }

  const skipWorker =
    process.env.TSSERVER_HOOK_NO_WORKER === '1' ||
    process.env.TSSERVER_HOOK_NO_WORKER === 'true';

  function receiveMap(msg) {
    if (msg.type !== 'resolution-map') return;
    cache.clear();
    for (const [key, value] of Object.entries(msg.data)) {
      cache.set(key, value);
    }
    ready = true;
    debug(`resolution map ready: ${cache.size} entries`);
    if (onUpdate) {
      try {
        onUpdate();
      } catch (e) {
        debug(`onUpdate failed: ${e.message}`);
      }
    }
  }

  if (!skipWorker && fs.existsSync(workerPath)) {
    try {
      worker = new Worker(workerPath, { workerData: { workspaceRoot } });
      worker.on('message', receiveMap);

      // Non-fatal: without a map every lookup misses and standard resolution stands.
      worker.on('error', (err) => debug(`worker error: ${err.message}`));
      worker.on('exit', (code) => {
        if (code !== 0) debug(`worker exited with code ${code}`);
      });
      // Message listeners re-reference the worker; unref last so one-shot callers can exit.
      worker.unref();
    } catch (e) {
      debug(`failed to spawn worker: ${e.message}`);
    }
  } else if (!skipWorker) {
    debug(`worker not found at ${workerPath} — Bazel resolution disabled`);
  }

  function lookup(moduleName) {
    if (!ready) return undefined;

    const direct = cache.get(moduleName);
    return direct && fs.existsSync(direct) ? direct : undefined;
  }

  return {
    dispose() {
      const active = worker;
      worker = undefined;
      active?.off('message', receiveMap);
      cache.clear();
      return active?.terminate();
    },
    resolve(moduleName) {
      const hit = lookup(moduleName);
      return hit ? buildResolvedModule(hit) : undefined;
    },
  };
}

module.exports = { createResolutionSource, findWorkspaceRoot };

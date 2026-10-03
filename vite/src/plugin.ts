/**
 * vite-plugin-bazel — main plugin implementation.
 *
 * Architecture
 * ────────────
 * `vite build`: Bazel pre-compiled everything to .js under bazel-bin, and the
 * plugin redirects every first-party .ts import there. Bazel owns the
 * transform; Vite only links.
 *
 * `vite dev`: Bazel is out of the inner loop. Checked-in source is handed to
 * Vite, which transforms it in memory — save-to-HMR without a Bazel analysis
 * and action cycle in between. bazel-bin is still the source of truth for what
 * Vite cannot produce: `ts_codegen` output, published assets, and the npm tree.
 *
 * Serving source means the dev server no longer typechecks. That is native
 * parity, not a regression — Vite has never typechecked, tsserver does — but it
 * makes editor correctness load-bearing: a type error now shows up in the
 * editor and in `bazel build`, and no longer blocks the browser update.
 *
 * The hooks:
 *
 *  1. resolveId  — decide, per import, whether Vite or bazel-bin owns the file.
 *
 *  2. load       — read a pre-compiled .js from bazel-bin and attach its
 *                  .js.map. Source files are not loaded here; Vite's own
 *                  pipeline transforms them.
 *
 *  3. config     — allow serving from bazel-bin and the Bazel node_modules.
 *
 *  4. configureServer — a bazel-bin watcher, so a rebuild of generated code
 *                  reaches the browser as HMR, and a config-input watcher, so a
 *                  rebuild that changed the server's own configuration restarts
 *                  it instead of leaving it running against a stale graph.
 *
 *  5. closeBundle  — detach both watchers when the server shuts down.
 */

import fs from 'node:fs';
import path from 'node:path';
import { resolve as resolvePackage } from 'resolve.exports';
import type {
  Plugin,
  ResolvedConfig,
  ViteDevServer,
  UserConfig,
  ConfigEnv,
  ModuleNode,
} from 'vite';
import { BazelResolver, type ResolverMode, type ResolverOptions } from './resolver.js';
import { BazelWatcher, ConfigWatcher, type ConfigInput } from './watcher.js';

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

export interface BazelPluginOptions {
  workspaceRoot?: string;

  declaredFiles?: ResolverOptions['declaredFiles'];

  bazelBin?: string;

  nodeModules?: string;

  /**
   * Bazel workspace name (the `name` attribute in MODULE.bazel / WORKSPACE).
   *
   * Currently unused at runtime but reserved for future runfiles-style path
   * construction.  Example: `"my_workspace"`.
   */
  workspace?: string;

  /**
   * Debounce window (ms) for aggregating ibazel rebuild events before
   * triggering HMR.
   *
   * Default: `50`.
   */
  hmrDebounceMs?: number;

  /**
   * `true` turns a watcher that fails to start into a hard error instead of a
   * warning; `false` skips the bazel-bin watcher entirely.  Unset is
   * best-effort: the dev server still boots, with a warning, and no HMR.
   */
  hmr?: boolean;

  /**
   * Inputs the generated config was produced from. A rebuild that changes one
   * of these restarts the dev server; a rebuild that changes only `ts_codegen`
   * output does not. `ts_dev_server` fills this in; a hand-written config that
   * leaves it empty gets no restart behaviour.
   */
  configInputs?: ConfigInput[];

  /**
   * Overrides the mode the resolver runs in. Normally taken from Vite itself
   * (`serve` under `vite dev`, `build` under `vite build`); set it only to pin
   * one mode in a test.
   */
  mode?: ResolverMode;
}

// ---------------------------------------------------------------------------
// Plugin factory
// ---------------------------------------------------------------------------

export function bazelPlugin(options: BazelPluginOptions = {}): Plugin {
  // ── State ────────────────────────────────────────────────────────────────
  // These are set in configResolved (guaranteed to run before resolveId /
  // load / configureServer).  Definite-assignment assertions reflect that.
  let bazelBinAbsolute!: string;
  let nodeModulesAbsolute: string | null = null;
  let resolver!: BazelResolver;
  let watcher: BazelWatcher | null = null;
  let configWatcher: ConfigWatcher | null = null;
  let mode: ResolverMode = options.mode ?? 'build';

  // ── Helpers ───────────────────────────────────────────────────────────────

  /**
   * Resolve the bazel-bin path to an absolute path.
   */
  function resolveBazelBin(root: string): string {
    const raw = options.bazelBin ?? 'bazel-bin';
    return path.isAbsolute(raw) ? raw : path.resolve(options.workspaceRoot ?? root, raw);
  }

  function resolveNodeModules(root: string): string | null {
    if (options.nodeModules == null) return null;
    const nm = options.nodeModules;
    return path.isAbsolute(nm) ? nm : path.resolve(options.workspaceRoot ?? root, nm);
  }

  function resolveRequest(
    activeResolver: BazelResolver,
    id: string,
    importer: string | undefined,
    extensions: readonly string[] | undefined,
    root: string | undefined,
  ) {
    let request = id;
    let serveRoot = activeResolver.mode === 'serve' && id.startsWith('/') && !id.startsWith('//')
      && !id.startsWith('/virtual:') && !id.includes('\0')
      ? root
      : undefined;
    if (activeResolver.mode === 'serve' && id.startsWith('/@fs/')) {
      const cut = id.search(/[?#]/);
      const suffix = cut < 0 ? '' : id.slice(cut);
      const filePath = cut < 0 ? id.slice(5) : id.slice(5, cut);
      const file = path.posix.normalize(path.sep === '\\' ? filePath.replace(/\\/g, '/') : filePath);
      request = (file.startsWith('/') || /^[a-z]:/i.test(file) ? file : '/' + file) + suffix;
      serveRoot = undefined;
    }
    const result = activeResolver.resolveId(request, importer, extensions, serveRoot);
    // Vite resolves an undeclared /@fs URL to the canonical id its own file watcher covers.
    if (request !== id && result !== null && !activeResolver.isDeclaredFile(result.filePath)) return null;
    return result;
  }

  // ── Plugin object ─────────────────────────────────────────────────────────

  return {
    name: 'vite-plugin-bazel',
    // Enforce runs before Vite's built-in resolvers so we can intercept .ts
    // imports before Vite tries (and fails) to find them.
    enforce: 'pre',

    // ── config ──────────────────────────────────────────────────────────────
    config(userConfig: UserConfig, env: ConfigEnv): UserConfig {
      mode = options.mode ?? (env.command === 'serve' ? 'serve' : 'build');

      const root = userConfig.root != null
        ? path.resolve(userConfig.root)
        : process.cwd();

      const bazelBin = resolveBazelBin(root);
      const nodeModules = resolveNodeModules(root);

      const patch: UserConfig = {
        server: {
          fs: {
            allow: [
              root,
              ...(options.workspaceRoot != null ? [options.workspaceRoot] : []),
              bazelBin,
              ...Object.values(options.declaredFiles ?? {}).map((file) => file.path),
              ...(nodeModules != null ? [nodeModules] : []),
            ],
          },
          watch: { ignored: [bazelBin] },
        },
        // Optimise dependencies from the importer's node_modules.
        optimizeDeps: {
          ...(nodeModules != null
            ? { include: [], exclude: [] }
            : {}),
        },
      };

      return patch;
    },

    // ── configResolved ────────────────────────────────────────────────────
    configResolved(config: ResolvedConfig): void {
      bazelBinAbsolute = resolveBazelBin(config.root);
      nodeModulesAbsolute = resolveNodeModules(config.root);

      resolver = new BazelResolver({
        workspaceRoot: options.workspaceRoot ?? config.root,
        bazelBin: bazelBinAbsolute,
        workspace: options.workspace,
        declaredFiles: options.declaredFiles,
        mode,
      });

      config.logger.info(`[vite-plugin-bazel] bazel-bin: ${bazelBinAbsolute}`);
      config.logger.info(
        mode === 'serve'
          ? '[vite-plugin-bazel] serving first-party source; Bazel is out of the ' +
              'inner loop, so the dev server does not typecheck (tsserver and ' +
              '`bazel build` do)'
          : '[vite-plugin-bazel] serving Bazel-compiled .js from bazel-bin',
      );
      if (nodeModulesAbsolute != null) {
        config.logger.info(
          `[vite-plugin-bazel] node_modules: ${nodeModulesAbsolute}`,
        );
      }
    },

    // ── resolveId ─────────────────────────────────────────────────────────
    resolveId: {
      filter: { id: /.*/ },
      handler(id, importer, resolveOptions) {
        const extensions = this.environment?.config.resolve.extensions;
        let request = id;
        let from = importer;
        let packageRequest = false;
        const source =
          importer !== undefined && !id.startsWith('.') && !path.isAbsolute(id)
            ? resolver.sourcePackage(importer)
            : undefined;
        if (source !== undefined) {
          const [specifier, suffix] = splitSuffix(id);
          const metadata = JSON.parse(
            fs.readFileSync(source.manifest, 'utf8').replace(/^\uFEFF/, ''),
          );
          if (
            specifier.startsWith('#') ||
            (metadata.exports != null &&
              typeof metadata.name === 'string' &&
              (specifier === metadata.name || specifier.startsWith(metadata.name + '/')))
          ) {
            const config = this.environment.config;
            const conditions = config.resolve.conditions.map((condition) =>
              condition === 'development|production'
                ? config.isProduction
                  ? 'production'
                  : 'development'
                : condition,
            );
            conditions.push(resolveOptions.kind === 'require-call' ? 'require' : 'import');
            this.addWatchFile(source.manifest);
            let target: string | undefined;
            try {
              target = resolvePackage(metadata, specifier, { conditions, unsafe: true })?.[0];
            } catch {
              // resolve.exports throws when the manifest maps no target; native resolution reports that.
            }
            request = target === undefined ? id : target + suffix;
            if (target?.startsWith('.')) {
              const selected = resolver.resolveId(request, source.manifest, extensions);
              request = selected?.directoryRequest ?? selected?.filePath ??
                path.resolve(path.dirname(source.manifest), target) + suffix;
            }
            from = source.importer;
            packageRequest = true;
          }
        }
        const result = resolveRequest(resolver, request, from, extensions, this.environment?.config.root);
        const directory = result === null ? undefined : resolver.declaredDirectory(result.filePath);
        if (mode === 'build' || (directory === undefined && result?.directoryRequest === undefined && !packageRequest)) {
          return result === null ? null : result.filePath;
        }
        // Vite normalizes a query's `/../` into the selected path, so only the file part is delegated.
        const [file, suffix] = result === null ? [request, ''] : splitSuffix(result.directoryRequest ?? result.filePath);
        return this.resolve(file, from, { ...resolveOptions, skipSelf: true }).then((resolved) => {
          if (resolved === null) {
            if (result?.directoryRequest !== undefined && !resolver.isDeclaredFile(result.filePath)) return null;
            return result?.filePath ?? null;
          }
          if (directory !== undefined && resolver.declaredDirectory(resolved.id) !== directory) {
            throw new Error(
              `Bazel module ${file} resolved outside declared directory: ${resolved.id}`,
            );
          }
          if (
            result !== null &&
            directory === undefined &&
            result.directoryRequest === undefined &&
            resolver.isDeclaredFile(result.filePath)
          ) {
            return { ...resolved, id: result.filePath };
          }
          if (resolved.external) return resolved;
          const selected = resolver.resolveId(resolved.id, undefined, extensions);
          return { ...resolved, id: (selected?.filePath ?? resolved.id) + suffix };
        });
      },
    },

    // ── load ──────────────────────────────────────────────────────────────
    load(id: string): { code: string; map?: string | null } | null {
      if (/[?#]/.test(id)) return null;
      const declared = resolver.isDeclaredFile(id);
      if (!id.endsWith('.js') || !resolver.isBazelOutput(id)) {
        // Vite reports a missing file as ERR_LOAD_URL and falls through to serving the checkout path.
        if (declared) fs.statSync(id);
        return null;
      }

      let code: string;
      try {
        code = fs.readFileSync(id, 'utf8');
      } catch (error) {
        if (declared) throw error;
        // File doesn't exist yet (build hasn't run for this target).
        return null;
      }

      // Locate the companion .js.map file.
      const mapPath = resolver.findMapForJs(id);
      let map: string | null = null;
      if (mapPath !== null) {
        try {
          map = fs.readFileSync(mapPath, 'utf8');
        } catch {
          // Map file disappeared between the existence check and the read;
          // continue without it.
        }
      }

      return { code, map };
    },

    // ── configureServer ───────────────────────────────────────────────────
    //
    // Returns nothing, and must keep returning nothing: Vite CALLS a function
    // returned from here as a post hook, so handing it a teardown closure
    // detaches both watchers before the first request, silently. Teardown lives
    // in closeBundle below.
    async configureServer(server: ViteDevServer): Promise<void> {
      await startConfigWatcher(server);

      if (options.hmr === false) return;

      watcher = new BazelWatcher({
        bazelBin: bazelBinAbsolute,
        debounceMs: options.hmrDebounceMs ?? 50,
        // Undeclared bazel-bin files the server already loaded, such as generated .ts, refresh too.
        isDeclaredFile: (file) =>
          resolver.isDeclaredOutput(file) || (server.moduleGraph.getModulesByFile(file)?.size ?? 0) > 0,
        declaredFiles: options.declaredFiles,
        onRebuild: (changedAbsolutePaths: Set<string>) => {
          handleRebuild(server, changedAbsolutePaths).catch((err: unknown) => {
            server.ws.send({
              type: 'error',
              err: { message: String(err), stack: err instanceof Error ? err.stack ?? '' : '' },
            });
          });
        },
      });

      try {
        await watcher.start();
      } catch (err: unknown) {
        watcher = null;
        const detail = err instanceof Error ? err.message : String(err);
        const message =
          `[vite-plugin-bazel] no HMR: the bazel-bin watcher failed to start — ${detail}`;
        if (options.hmr === true) throw new Error(message);
        server.config.logger.warn(message);
      }
    },

    // ── closeBundle ───────────────────────────────────────────────────────
    // Vite runs this when the dev server (or a build) shuts down.
    closeBundle(): void {
      if (watcher !== null) {
        watcher.stop().catch(() => {
          // Best-effort cleanup; ignore errors during shutdown.
        });
        watcher = null;
      }
      if (configWatcher !== null) {
        configWatcher.stop().catch(() => {
          // Best-effort cleanup; ignore errors during shutdown.
        });
        configWatcher = null;
      }
    },
  };

  /**
   * Watches the inputs `ts_dev_server` generated the config from. A rebuild
   * that leaves them all identical is a codegen-only rebuild and must not
   * disturb the running server.
   */
  async function startConfigWatcher(server: ViteDevServer): Promise<void> {
    const inputs = options.configInputs ?? [];
    if (inputs.length === 0) return;

    configWatcher = new ConfigWatcher({
      inputs,
      debounceMs: options.hmrDebounceMs ?? 50,
      onStale: (changed: ConfigInput[]) => {
        const names = changed.map((input) => input.label).join(', ');
        server.config.logger.info(
          `[vite-plugin-bazel] restarting: ${names} changed since this server started`,
        );
        for (const input of changed) {
          if (input.remedy !== 'manual') continue;
          server.config.logger.warn(
            `[vite-plugin-bazel] ${input.label} changed, which a restart of Vite ` +
              'cannot pick up — re-run `bazel run` on this target',
          );
        }
        server.restart().catch((err: unknown) => {
          const detail = err instanceof Error ? err.message : String(err);
          server.config.logger.error(`[vite-plugin-bazel] restart failed: ${detail}`);
        });
      },
    });
    await configWatcher.start();
  }
}

// ---------------------------------------------------------------------------
// HMR: handle a completed ibazel rebuild
// ---------------------------------------------------------------------------

async function handleRebuild(
  server: ViteDevServer,
  changedAbsolutePaths: Set<string>,
): Promise<void> {
  const cacheDir = canonicalPath(server.config.cacheDir);
  const modulesToUpdate = new Set<ModuleNode>();

  for (const absPath of changedAbsolutePaths) {
    const realPath = canonicalPath(absPath);
    if (realPath === cacheDir || realPath.startsWith(cacheDir + path.sep)) continue;
    // The resolved path is an alias only when the graph lacks the watched one: oj
    // synthesizes a node, with a second URL, for any existing file it is asked about.
    let modules = server.moduleGraph.getModulesByFile(absPath) ?? new Set<ModuleNode>();
    if (modules.size === 0) modules = server.moduleGraph.getModulesByFile(realPath) ?? new Set<ModuleNode>();
    if (modules.size === 0) {
      server.moduleGraph.invalidateAll();
      server.ws.send({ type: 'full-reload' });
      return;
    }
    for (const mod of modules) modulesToUpdate.add(mod);
  }

  if (typeof server.reloadModule === 'function') {
    for (const mod of modulesToUpdate) await server.reloadModule(mod);
    return;
  }
  // oj's ViteDevServer stand-in has no reloadModule.
  const timestamp = Date.now();
  for (const mod of modulesToUpdate) server.moduleGraph.invalidateModule(mod);
  server.ws.send({
    type: 'update',
    updates: [...modulesToUpdate].map((mod) => ({
      type: 'js-update' as const,
      path: mod.url,
      acceptedPath: mod.url,
      timestamp,
      explicitImportRequired: false,
      isWithinCircularImport: false,
    })),
  });
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function splitSuffix(request: string): [string, string] {
  const cut = request.slice(1).search(/[?#]/);
  return cut < 0 ? [request, ''] : [request.slice(0, cut + 1), request.slice(cut + 1)];
}

function canonicalPath(file: string): string {
  for (let ancestor = file; ; ancestor = path.dirname(ancestor)) {
    try {
      return path.resolve(fs.realpathSync(ancestor), path.relative(ancestor, file));
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if ((code !== 'ENOENT' && code !== 'ENOTDIR') || path.dirname(ancestor) === ancestor) {
        throw error;
      }
    }
  }
}

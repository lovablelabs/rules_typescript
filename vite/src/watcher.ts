import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import type { ResolverOptions } from './resolver.js';

/**
 * The part of Vite's `server.watcher` this module uses. Vite runs that watcher
 * already and hands it to every plugin; chokidar itself is bundled inside
 * Vite's dist and is not a package a sibling module can import.
 */
export interface WatchSource {
  add(paths: string): unknown;
  on(event: 'add' | 'change', listener: (filePath: string) => void): unknown;
  off?(event: 'add' | 'change', listener: (filePath: string) => void): unknown;
  unwatch?(paths: string): unknown;
}

export type RebuildCallback = (changedPaths: Set<string>) => void;

export interface BazelWatcherOptions {
  /** Absolute path to the bazel-bin output tree. */
  bazelBin: string;
  onRebuild: RebuildCallback;
  isDeclaredFile?: (file: string) => boolean;
  declaredFiles?: ResolverOptions['declaredFiles'];
  source?: WatchSource;
  /**
   * Quiet period before a batch is flushed, in milliseconds (default 50).
   * Bazel writes all outputs of a rebuild within a few milliseconds of each
   * other, and HMR latency should stay under 100 ms.
   */
  debounceMs?: number;
}

interface NativeWatch {
  readonly watcher: fs.FSWatcher;
  readonly dev: number;
  readonly ino: number;
  readonly directory: boolean;
}

export class BazelWatcher {
  private readonly bazelBin: string;
  private readonly onRebuild: RebuildCallback;
  private readonly source: WatchSource | null;
  private readonly isDeclaredFile: (file: string) => boolean;
  private readonly declaredFiles: NonNullable<ResolverOptions['declaredFiles']>;
  private readonly debounceMs: number;

  private readonly onFileEvent = (filePath: string): void => {
    this.handleFileEvent(filePath);
  };

  private readonly watches = new Map<string, NativeWatch>();
  private attached: WatchSource | null = null;
  private pendingChanges: Set<string> = new Set();
  private debounceTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(options: BazelWatcherOptions) {
    this.bazelBin = options.bazelBin;
    this.onRebuild = options.onRebuild;
    this.isDeclaredFile = options.isDeclaredFile ?? (() => false);
    this.declaredFiles = options.declaredFiles ?? {};
    this.source = options.source ?? null;
    this.debounceMs = options.debounceMs ?? 50;
  }

  async start(): Promise<void> {
    if (!fs.existsSync(this.bazelBin)) {
      throw new Error(
        `bazel-bin does not exist at ${this.bazelBin}. Build the target once ` +
          '(bazel build //your:target) before starting the dev server, or set the ' +
          'bazelBin option to the real output tree.',
      );
    }

    if (this.source !== null) {
      for (const root of this.watchRoots()) this.source.add(root);
      this.source.on('add', this.onFileEvent);
      this.source.on('change', this.onFileEvent);
      this.attached = this.source;
      return;
    }

    try {
      const realBin = fs.realpathSync(this.bazelBin);
      for (const root of this.watchRoots()) {
        if (root !== this.bazelBin &&
          (root === realBin || bazelPathToModuleId(root, realBin) !== null)) continue;
        this.watchEntry(root, false);
      }
    } catch (err) {
      await this.stop();
      const detail = err instanceof Error ? err.message : String(err);
      throw new Error(`cannot watch ${this.bazelBin} with node:fs.watch (${detail})`);
    }
  }

  /** Detaches every listener. An injected source is never closed — it is Vite's. */
  async stop(): Promise<void> {
    if (this.debounceTimer !== null) {
      clearTimeout(this.debounceTimer);
      this.debounceTimer = null;
    }
    if (this.attached !== null) {
      this.attached.off?.('add', this.onFileEvent);
      this.attached.off?.('change', this.onFileEvent);
      for (const root of this.watchRoots()) this.attached.unwatch?.(root);
      this.attached = null;
    }
    for (const resource of this.watches.values()) resource.watcher.close();
    this.watches.clear();
    this.pendingChanges.clear();
  }

  private *watchRoots(): Iterable<string> {
    yield this.bazelBin;
    for (const file of Object.values(this.declaredFiles)) {
      if (!file.isSource && bazelPathToModuleId(file.path, this.bazelBin) === null) yield path.dirname(file.path);
    }
  }

  private isWatchPath(filePath: string): boolean {
    return filePath === this.bazelBin ||
      bazelPathToModuleId(filePath, this.bazelBin) !== null || this.isDeclaredFile(filePath);
  }

  private canFollowDirectorySymlink(filePath: string): boolean {
    for (const root of this.watchRoots()) {
      if (filePath === root) return true;
    }
    const realPath = fs.realpathSync(filePath);
    return Object.values(this.declaredFiles).some((file) => {
      if (file.isSource || !file.directory) return false;
      try {
        const root = fs.realpathSync(file.path);
        return realPath === root || realPath.startsWith(root + path.sep);
      } catch (err) {
        if (!this.isUnavailableChild(file.path, err)) throw err;
        return false;
      }
    });
  }

  private hasWatchedAncestor(filePath: string, stat: fs.Stats): boolean {
    for (let ancestor = filePath; ancestor !== path.dirname(ancestor);) {
      ancestor = path.dirname(ancestor);
      const resource = this.watches.get(ancestor);
      if (resource?.dev === stat.dev && resource.ino === stat.ino) return true;
    }
    return false;
  }

  private watchEntry(filePath: string, notify: boolean): void {
    const previous = this.watches.get(filePath);
    try {
      const entry = fs.lstatSync(filePath, { throwIfNoEntry: false });
      const stat = entry?.isSymbolicLink()
        ? fs.statSync(filePath, { throwIfNoEntry: false })
        : entry;
      const directory =
        stat?.isDirectory() === true &&
        !this.hasWatchedAncestor(filePath, stat) &&
        (!entry?.isSymbolicLink() || filePath === this.bazelBin || this.canFollowDirectorySymlink(filePath));
      const needsWatch = stat !== undefined && (directory || (entry?.isSymbolicLink() && stat.isFile()));
      if (
        previous &&
        (!needsWatch || previous.dev !== stat?.dev ||
          previous.ino !== stat?.ino || previous.directory !== directory)
      ) {
        this.unwatchTree(filePath);
        if (notify && previous.directory) this.enqueueChange(filePath);
      }

      if (needsWatch && stat && !this.watches.has(filePath)) {
        const onChange: fs.WatchListener<string> = (_event, filename) => {
          if (this.watches.get(filePath) !== resource) return;
          this.watchEntry(filePath, true);
          if (!resource.directory || this.watches.get(filePath) !== resource) return;
          if (filename === null) {
            for (const watched of [...this.watches.keys()]) {
              if (path.dirname(watched) === filePath) this.watchEntry(watched, true);
            }
            this.scanDirectory(filePath, true);
            this.enqueueChange(filePath);
          } else {
            const changed = path.resolve(filePath, filename.toString());
            // oj's Deno-based plugin host reports a nested change by basename to every ancestor directory watch.
            const known = this.watches.has(changed) || this.isDeclaredFile(changed) ||
              fs.lstatSync(changed, { throwIfNoEntry: false }) !== undefined;
            if (known && this.isWatchPath(changed)) this.watchEntry(changed, true);
          }
        };
        const watcher = fs.watch(filePath, { persistent: true }, onChange);
        const resource: NativeWatch = { watcher, dev: stat.dev, ino: stat.ino, directory };
        this.watches.set(filePath, resource);
        const current = fs.statSync(filePath, { throwIfNoEntry: false });
        if (current?.dev !== stat.dev || current?.ino !== stat.ino) {
          this.unwatchTree(filePath);
          if (filePath === this.bazelBin) {
            throw new Error(`watch identity changed while attaching to ${filePath}`);
          }
          // The parent directory watch observes the replacement and installs its current entry.
          if (notify) this.enqueueChange(filePath);
          return;
        }
        if (directory) this.scanDirectory(filePath, notify);
      }
      if (notify && !directory) this.handleFileEvent(filePath);
    } catch (err) {
      if (!this.isUnavailableChild(filePath, err)) throw err;
      this.unwatchTree(filePath);
      if (notify) {
        if (previous?.directory) this.enqueueChange(filePath);
        else this.handleFileEvent(filePath);
      }
    }
  }

  private isUnavailableChild(filePath: string, err: unknown): boolean {
    const code = (err as NodeJS.ErrnoException).code;
    return (
      filePath !== this.bazelBin &&
      (code === 'ENOENT' || code === 'ENOTDIR' || code === 'ELOOP')
    );
  }

  private scanDirectory(directory: string, notify: boolean): void {
    let names: string[];
    try {
      names = fs.readdirSync(directory);
    } catch (err) {
      if (!this.isUnavailableChild(directory, err)) throw err;
      this.unwatchTree(directory);
      if (notify) this.enqueueChange(directory);
      return;
    }
    for (const name of names) {
      const child = path.join(directory, name);
      if (this.isWatchPath(child)) this.watchEntry(child, notify);
    }
  }

  private unwatchTree(directory: string): void {
    for (const [watched, resource] of this.watches) {
      if (watched === directory || watched.startsWith(directory + path.sep)) {
        this.watches.delete(watched);
        resource.watcher.close();
      }
    }
  }

  private handleFileEvent(absolutePath: string): void {
    if (!absolutePath.endsWith('.js') && !this.isDeclaredFile(absolutePath)) return;
    this.enqueueChange(absolutePath);
  }

  private enqueueChange(absolutePath: string): void {
    if (bazelPathToModuleId(absolutePath, this.bazelBin) === null && !this.isDeclaredFile(absolutePath)) return;

    this.pendingChanges.add(absolutePath);
    this.scheduleFlush();
  }

  private scheduleFlush(): void {
    if (this.debounceTimer !== null) {
      clearTimeout(this.debounceTimer);
    }
    this.debounceTimer = setTimeout(() => {
      this.flush();
    }, this.debounceMs);
  }

  private flush(): void {
    this.debounceTimer = null;

    if (this.pendingChanges.size === 0) return;

    // Snapshot before the callback so events arriving during it are not lost.
    const snapshot = new Set(this.pendingChanges);
    this.pendingChanges.clear();

    this.onRebuild(snapshot);
  }
}

/** One watched input and the digest it had when Vite last read it. */
export interface ConfigInput {
  /** Human-readable name used in the restart message. */
  label: string;
  /** Absolute path whose fingerprint is watched. */
  path: string;
  /**
   * `content` hashes the bytes. `identity` resolves the symlink instead, for
   * inputs too large to hash whose Bazel path already encodes their version.
   */
  digest: 'content' | 'identity';
  /**
   * `restart` is fixable in-process: Vite re-reads it. `manual` is not — a
   * different Node binary needs a new `bazel run`.
   */
  remedy: 'restart' | 'manual';
}

/** Named inputs that changed, in the order they were declared. */
export type StaleCallback = (changed: ConfigInput[]) => void;

export interface ConfigWatcherOptions {
  inputs: ConfigInput[];
  onStale: StaleCallback;
  /** Quiet period before the fingerprint is recomputed (default 50 ms). */
  debounceMs?: number;
}

// Watches the inputs that generated the running Vite config, which Vite itself
// does not know of. Digests, not mtimes: each action run rewrites its outputs.
export class ConfigWatcher {
  private readonly inputs: ConfigInput[];
  private readonly onStale: StaleCallback;
  private readonly debounceMs: number;

  private digests: Map<string, string> = new Map();
  private debounceTimer: ReturnType<typeof setTimeout> | null = null;
  private watchers: fs.FSWatcher[] = [];

  private readonly onFileEvent = (): void => {
    this.scheduleCheck();
  };

  constructor(options: ConfigWatcherOptions) {
    this.inputs = options.inputs;
    this.onStale = options.onStale;
    this.debounceMs = options.debounceMs ?? 50;
  }

  /** The digest of every input as of now, keyed by path. */
  snapshot(): Map<string, string> {
    const digests = new Map<string, string>();
    for (const input of this.inputs) {
      digests.set(input.path, digestOf(input.path, input.digest));
    }
    return digests;
  }

  async start(): Promise<void> {
    this.digests = this.snapshot();
    const directories = new Map<string, Set<string>>();
    for (const input of this.inputs) {
      const directory = path.dirname(input.path);
      const names = directories.get(directory) ?? new Set<string>();
      names.add(path.basename(input.path));
      directories.set(directory, names);
    }
    try {
      for (const [directory, names] of directories) {
        this.watchers.push(fs.watch(directory, (_event, filename) => {
          if (filename === null || names.has(filename.toString())) this.onFileEvent();
        }));
      }
    } catch (err) {
      await this.stop();
      throw err;
    }
  }

  async stop(): Promise<void> {
    if (this.debounceTimer !== null) {
      clearTimeout(this.debounceTimer);
      this.debounceTimer = null;
    }
    for (const watcher of this.watchers) watcher.close();
    this.watchers = [];
  }

  /**
   * Recomputes every digest and reports the inputs that moved. Exposed so the
   * decision can be tested, and driven directly on a watcher event.
   */
  check(): ConfigInput[] {
    const next = this.snapshot();
    const changed = this.inputs.filter(
      (input) => next.get(input.path) !== this.digests.get(input.path),
    );
    this.digests = next;
    return changed;
  }

  private scheduleCheck(): void {
    if (this.debounceTimer !== null) clearTimeout(this.debounceTimer);
    this.debounceTimer = setTimeout(() => {
      this.debounceTimer = null;
      const changed = this.check();
      if (changed.length > 0) this.onStale(changed);
    }, this.debounceMs);
  }
}

/** Fingerprint of one input, or a sentinel when it cannot be read. */
export function digestOf(filePath: string, kind: 'content' | 'identity' = 'content'): string {
  try {
    if (kind === 'identity') return fs.realpathSync(filePath);
    return crypto.createHash('sha256').update(fs.readFileSync(filePath)).digest('hex');
  } catch {
    return 'absent';
  }
}

/**
 * Converts an absolute .js path under bazel-bin to a Vite module ID of the form
 * `/workspace/relative/path.js`, or null when it is not under bazelBin.
 */
export function bazelPathToModuleId(absolutePath: string, bazelBin: string): string | null {
  const rel = path.relative(bazelBin, absolutePath);
  if (rel === '' || rel.startsWith('..') || path.isAbsolute(rel)) return null;
  return '/' + rel.split(path.sep).join('/');
}

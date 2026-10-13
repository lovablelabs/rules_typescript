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
  /**
   * Watch directory roots recursively, subdirectories sharing the root's handle. Default: Node on darwin,
   * where libuv recreates its one FSEvents stream per event loop on every directory watch opened or closed,
   * dropping events in flight (root and directory-symlink handles still do). Deno's fs.watch reports basenames.
   */
  recursiveWatch?: boolean;
}

type Closable = Pick<fs.FSWatcher, 'close'>;

/** Node on darwin: libuv runs one FSEvents stream per loop and restarts it for every directory handle. */
const LIBUV_FSEVENTS = process.platform === 'darwin' && !('Deno' in globalThis);

interface NativeWatch {
  /** Null for a directory its parent's recursive watch already covers. */
  readonly watcher: Closable | null;
  readonly onEvent: (filename: string | null) => void;
  readonly dev: number;
  readonly ino: number;
  readonly directory: boolean;
  /** A link's resolved target outside bazel-bin. A change to it re-watches the link. */
  readonly target: string | null;
}

interface TargetDirectory {
  readonly watcher: fs.FSWatcher;
  readonly dev: number;
  readonly ino: number;
  /** The links into the directory, by target name. */
  readonly links: Map<string, Set<string>>;
}

export class BazelWatcher {
  private readonly bazelBin: string;
  private readonly onRebuild: RebuildCallback;
  private readonly source: WatchSource | null;
  private readonly isDeclaredFile: (file: string) => boolean;
  private readonly declaredFiles: NonNullable<ResolverOptions['declaredFiles']>;
  private readonly debounceMs: number;
  private readonly recursive: boolean;
  private readonly roots: readonly string[];
  private readonly rootSet: ReadonlySet<string>;
  private readonly declaredDirectories: readonly string[];
  /** Declared outputs under bazel-bin, at their bazel-bin path, and every directory above them. */
  private declaredBinPaths = new Set<string>();
  private declaredBinTrees = new Set<string>();
  /** Directories left unwatched at start, by device and inode. */
  private readonly skipped = new Map<string, string>();
  private realBin: string | null = null;
  private readonly loadedDirectories = new Map<string, string | null>();

  private readonly onFileEvent = (filePath: string): void => {
    this.handleFileEvent(filePath);
  };

  private readonly watches = new Map<string, NativeWatch>();
  private readonly fileStamps = new Map<string, string>();
  private readonly targetDirectories = new Map<string, TargetDirectory>();
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
    this.recursive = options.recursiveWatch ?? LIBUV_FSEVENTS;
    const outputs = Object.values(this.declaredFiles).filter((file) => !file.isSource);
    const outside = outputs.filter((file) => bazelPathToModuleId(file.path, this.bazelBin) === null);
    this.roots = [this.bazelBin, ...outside.map((file) => path.dirname(file.path))];
    this.rootSet = new Set(this.roots);
    this.declaredDirectories = outputs.filter((file) => file.directory).map((file) => file.path);
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
      for (const root of this.roots) this.source.add(root);
      this.source.on('add', this.onFileEvent);
      this.source.on('change', this.onFileEvent);
      this.attached = this.source;
      return;
    }

    try {
      const realBin = fs.realpathSync(this.bazelBin);
      this.realBin = realBin;
      this.indexDeclaredBinPaths(realBin);
      for (const root of this.roots) {
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
      for (const root of this.roots) this.attached.unwatch?.(root);
      this.attached = null;
    }
    for (const resource of this.watches.values()) resource.watcher?.close();
    this.watches.clear();
    this.fileStamps.clear();
    this.skipped.clear();
    this.pendingChanges.clear();
  }

  private indexDeclaredBinPaths(realBin: string): void {
    this.declaredBinPaths = new Set([this.bazelBin]);
    this.declaredBinTrees = new Set();
    for (const file of Object.values(this.declaredFiles)) {
      if (file.isSource) continue;
      const moduleId = bazelPathToModuleId(file.path, this.bazelBin) ?? bazelPathToModuleId(file.path, realBin);
      if (moduleId === null) continue;
      const binPath = path.join(this.bazelBin, moduleId);
      if (file.directory) this.declaredBinTrees.add(binPath);
      for (let ancestor = binPath; !this.declaredBinPaths.has(ancestor); ancestor = path.dirname(ancestor)) {
        this.declaredBinPaths.add(ancestor);
      }
    }
  }

  /**
   * With outputs declared under bazel-bin, a directory there that leads to none of them is not watched at start:
   * on a large application that was most of bazel-bin. One that appears or is replaced later still is.
   */
  private skips(directory: string, stat: fs.Stats, notify: boolean): boolean {
    const identity = `${stat.dev}:${stat.ino}`;
    if (notify) return this.skipped.get(directory) === identity;
    if (this.declaredBinPaths.size <= 1 || this.declaredBinPaths.has(directory)) return false;
    if (bazelPathToModuleId(directory, this.bazelBin) === null) return false;
    for (let ancestor = directory; ancestor.length > this.bazelBin.length; ancestor = path.dirname(ancestor)) {
      if (this.declaredBinTrees.has(ancestor)) return false;
    }
    this.skipped.set(directory, identity);
    return true;
  }

  /** Watches the directories of a file the server loaded from under bazel-bin, if start skipped them. */
  watchLoaded(file: string): void {
    if (this.skipped.size === 0 || this.realBin === null) return;
    const directory = path.dirname(file);
    let binDirectory = this.loadedDirectories.get(directory);
    if (binDirectory === undefined) {
      binDirectory = this.binPath(directory, this.realBin);
      this.loadedDirectories.set(directory, binDirectory);
    }
    if (binDirectory === null) return;
    let skippedAncestor: string | undefined;
    for (let ancestor = binDirectory; !this.declaredBinPaths.has(ancestor);) {
      this.declaredBinPaths.add(ancestor);
      if (this.skipped.delete(ancestor)) skippedAncestor = ancestor;
      ancestor = path.dirname(ancestor);
    }
    if (skippedAncestor === undefined) return;
    try {
      this.watchEntry(skippedAncestor, false);
    } catch {
      // A watch that cannot attach costs this file its HMR, not its load.
    }
  }

  /** A directory's path under the bazel-bin link, or null when it does not resolve under bazel-bin. */
  private binPath(directory: string, realBin: string): string | null {
    let moduleId = bazelPathToModuleId(directory, this.bazelBin) ?? bazelPathToModuleId(directory, realBin);
    if (moduleId === null) {
      try {
        moduleId = bazelPathToModuleId(fs.realpathSync(directory), realBin);
      } catch {
        return null;
      }
    }
    return moduleId === null ? null : path.join(this.bazelBin, moduleId);
  }

  private isWatchPath(filePath: string): boolean {
    return filePath === this.bazelBin ||
      bazelPathToModuleId(filePath, this.bazelBin) !== null || this.isDeclaredFile(filePath);
  }

  private canFollowDirectorySymlink(filePath: string): boolean {
    if (this.rootSet.has(filePath)) return true;
    const realPath = fs.realpathSync(filePath);
    return this.declaredDirectories.some((directory) => {
      try {
        const root = fs.realpathSync(directory);
        return realPath === root || realPath.startsWith(root + path.sep);
      } catch (err) {
        if (!this.isUnavailableChild(directory, err)) throw err;
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
        (!entry?.isSymbolicLink() || filePath === this.bazelBin || this.canFollowDirectorySymlink(filePath)) &&
        !this.skips(filePath, stat, notify);
      const needsWatch = stat !== undefined && (directory || (entry?.isSymbolicLink() && stat.isFile()));
      const target = needsWatch && !directory ? this.watchedTarget(filePath) : null;
      if (
        previous &&
        (!needsWatch || previous.dev !== stat?.dev || previous.ino !== stat?.ino ||
          previous.directory !== directory || previous.target !== target)
      ) {
        this.unwatchTree(filePath);
        if (notify && previous.directory) this.enqueueChange(filePath);
      }

      if (needsWatch && stat && !this.watches.has(filePath)) {
        const onEvent = (filename: string | null): void => {
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
            const changed = path.resolve(filePath, filename);
            // oj's Deno-based plugin host reports a nested change by basename to every ancestor directory watch.
            const known = this.watches.has(changed) || this.isDeclaredFile(changed) ||
              fs.lstatSync(changed, { throwIfNoEntry: false }) !== undefined;
            if (known && this.isWatchPath(changed)) this.watchEntry(changed, true);
          }
        };
        const covered = this.recursive && directory && !entry?.isSymbolicLink() &&
          this.watches.get(path.dirname(filePath))?.directory === true;
        let watcher: Closable | null = null;
        if (target !== null) watcher = this.watchLinkTarget(filePath, target, onEvent);
        else if (!covered) watcher = this.watchPath(filePath, directory, onEvent);
        const resource: NativeWatch = { watcher, onEvent, dev: stat.dev, ino: stat.ino, directory, target };
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
      if (!directory) this.reportFile(filePath, stat, notify);
    } catch (err) {
      if (!this.isUnavailableChild(filePath, err)) throw err;
      this.fileStamps.delete(filePath);
      this.unwatchTree(filePath);
      if (notify) {
        if (previous?.directory) this.enqueueChange(filePath);
        else this.handleFileEvent(filePath);
      }
    }
  }

  private watchPath(filePath: string, directory: boolean, onEvent: NativeWatch['onEvent']): fs.FSWatcher {
    return fs.watch(
      filePath,
      { persistent: true, recursive: this.recursive && directory },
      (_event, filename) => this.dispatch(filePath, filename === null ? null : filename.toString(), onEvent),
    );
  }

  /**
   * The target to watch a link through, when it is a file outside bazel-bin: a watch on the link follows it to an
   * inode whose link count a remote executor's co-located actions change for every input they stage.
   */
  private watchedTarget(link: string): string | null {
    if (LIBUV_FSEVENTS) return null;
    const target = fs.realpathSync(link);
    // start() resolves realBin before the first watch. A target inside it is an output Bazel writes in place.
    return bazelPathToModuleId(target, this.realBin!) === null ? target : null;
  }

  /** Shares one watch of the target's directory among every link into it. */
  private watchLinkTarget(link: string, target: string, onEvent: NativeWatch['onEvent']): Closable {
    const directory = path.dirname(target);
    let shared = this.targetDirectories.get(directory);
    if (shared === undefined) {
      const identity = fs.statSync(directory);
      let watcher: fs.FSWatcher;
      try {
        watcher = fs.watch(directory, { persistent: true }, (_event, filename) => {
          const name = filename === null ? null : path.basename(filename.toString());
          this.onTargetDirectoryEvent(directory, created, name);
        });
      } catch (err) {
        const code = (err as NodeJS.ErrnoException).code;
        // inotify needs read access to the directory, not only to the target in it.
        if (code === 'EACCES' || code === 'EPERM') return this.watchPath(link, false, onEvent);
        throw err;
      }
      const created: TargetDirectory = { watcher, dev: identity.dev, ino: identity.ino, links: new Map() };
      this.targetDirectories.set(directory, (shared = created));
    }
    const owner = shared;
    const name = path.basename(target);
    owner.links.set(name, (owner.links.get(name) ?? new Set()).add(link));
    return {
      close: () => {
        const named = owner.links.get(name);
        named?.delete(link);
        if (named?.size === 0) owner.links.delete(name);
        if (owner.links.size > 0 || this.targetDirectories.get(directory) !== owner) return;
        owner.watcher.close();
        this.targetDirectories.delete(directory);
      },
    };
  }

  /** Wakes the links an event names. A replaced or removed directory re-attaches every link into it. */
  private onTargetDirectoryEvent(directory: string, shared: TargetDirectory, name: string | null): void {
    if (this.targetDirectories.get(directory) !== shared) return;
    // libuv reports an event on the watched directory itself under the directory's basename.
    if (name === null || name === path.basename(directory)) {
      const current = fs.statSync(directory, { throwIfNoEntry: false });
      if (current?.dev !== shared.dev || current?.ino !== shared.ino) {
        shared.watcher.close();
        this.targetDirectories.delete(directory);
        for (const link of [...shared.links.values()].flatMap((links) => [...links])) {
          this.unwatchTree(link);
          this.watchEntry(link, true);
        }
        return;
      }
    }
    const names = name === null ? [...shared.links.keys()] : [name];
    for (const changed of names) {
      for (const link of [...(shared.links.get(changed) ?? [])]) this.watches.get(link)?.onEvent(null);
    }
  }

  /** Stamps a reportable file at every sighting, so the walk at start records what later events compare to. */
  private reportFile(filePath: string, stat: fs.Stats | undefined, notify: boolean): void {
    const stamped = stat?.isDirectory() !== true && this.isReportable(filePath);
    if (stamped && !this.stampChanged(filePath, stat)) return;
    if (notify) this.handleFileEvent(filePath);
  }

  /** Leaves out ctime and link count: remote executors hardlink inputs from a cache co-located actions share. */
  private stampChanged(filePath: string, stat: fs.Stats | undefined): boolean {
    if (stat === undefined) {
      this.fileStamps.delete(filePath);
      return true;
    }
    const stamp = `${stat.dev}:${stat.ino}:${stat.size}:${stat.mtimeMs}`;
    if (this.fileStamps.get(filePath) === stamp) return false;
    this.fileStamps.set(filePath, stamp);
    return true;
  }

  /** Routes a recursive watch's nested event to the nearest watched directory above it. */
  private dispatch(root: string, filename: string | null, own: NativeWatch['onEvent']): void {
    if (filename === null) {
      for (const [watched, resource] of [...this.watches]) {
        if (resource.watcher === null && resource.directory && watched.startsWith(root + path.sep)) {
          resource.onEvent(null);
        }
      }
      own(null);
      return;
    }
    let name = path.basename(filename);
    for (let directory = path.dirname(path.resolve(root, filename)); directory.startsWith(root + path.sep);) {
      const resource = this.watches.get(directory);
      if (resource?.directory) {
        const changed = path.join(directory, name);
        if (!this.watches.has(changed) && !changed.endsWith('.js') && !this.isDeclaredFile(changed)) {
          const entry = fs.lstatSync(changed, { throwIfNoEntry: false });
          if (!entry?.isDirectory() && !entry?.isSymbolicLink()) return;
        }
        resource.onEvent(name);
        return;
      }
      name = path.basename(directory);
      directory = path.dirname(directory);
    }
    own(name);
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
    let tree = false;
    for (const [watched, resource] of this.watches) {
      if (watched === directory || watched.startsWith(directory + path.sep)) {
        this.watches.delete(watched);
        resource.watcher?.close();
        tree ||= resource.directory;
      }
    }
    if (!tree) return;
    for (const stamped of this.fileStamps.keys()) {
      if (stamped.startsWith(directory + path.sep)) this.fileStamps.delete(stamped);
    }
  }

  private isReportable(filePath: string): boolean {
    return filePath.endsWith('.js') || this.isDeclaredFile(filePath);
  }

  private handleFileEvent(absolutePath: string): void {
    if (!this.isReportable(absolutePath)) return;
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

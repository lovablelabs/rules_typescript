/**
 * Path resolution for vite-plugin-bazel.
 *
 * Two modes, because dev and prod want opposite things from the same import.
 *
 * `build` — every first-party `.ts`/`.tsx` import is redirected to the
 * pre-compiled `.js` Bazel already wrote under bazel-bin. Bazel owns the
 * transform; Vite only links. This is the mode the plugin has always had, and
 * `resolveIdForBuild` below is that code unchanged.
 *
 * `serve` — checked-in first-party source is handed back to Vite to transform
 * in memory, which is what takes a Bazel analysis+action cycle out of the
 * keystroke-to-browser path. bazel-bin stays authoritative for what Vite
 * cannot produce itself: `ts_codegen` outputs (route trees, generated protos)
 * and published assets.
 */

import fs from 'node:fs';
import path from 'node:path';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/** Which half of the ruleset is driving: `vite dev` or `vite build`. */
export type ResolverMode = 'serve' | 'build';

export interface ResolverOptions {
  workspaceRoot: string;
  declaredFiles?: Readonly<Record<string, {
    readonly path: string;
    readonly context: 'source' | 'asset';
    readonly isSource?: boolean;
    readonly directory?: boolean;
    readonly importer?: string;
    readonly scope?: string;
  }>> | undefined;
  /** Absolute path to the bazel-bin output tree. */
  bazelBin: string;
  /** Optional Bazel workspace name (currently unused but reserved for
   *  runfiles-style path construction in a future iteration). */
  workspace?: string | undefined;
  /** Defaults to `build`: redirect everything to bazel-bin, as before. */
  mode?: ResolverMode | undefined;
}

export interface ResolvedFile {
  /** Absolute path to the .js file under bazel-bin. */
  jsPath: string;
  /** Absolute path to the .js.map file, or null if it does not exist. */
  mapPath: string | null;
}

type DeclaredFile = NonNullable<ResolverOptions['declaredFiles']>[string];

/** Where an import specifier landed, and who is expected to transform it. */
export interface Resolution {
  /** Absolute path of the file Vite should load. */
  filePath: string;
  directoryRequest?: string;
  /**
   * True when `filePath` is a Bazel `.js` output to be served verbatim with its
   * `.js.map`; false when it is source for Vite to transform.
   */
  precompiled: boolean;
  /** Absolute `.js.map` path, when one exists next to a precompiled output. */
  mapPath: string | null;
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/**
 * Returns true when the import ID is definitely a relative path (starts with
 * `.` or `..`) rather than a bare specifier or absolute path.
 */
export function isRelativeImport(id: string): boolean {
  return id.startsWith('./') || id.startsWith('../');
}

/**
 * Returns true when the path looks like a TypeScript source file that should
 * be intercepted and redirected to its bazel-bin counterpart.
 *
 * We catch explicit .ts / .tsx extensions but explicitly exclude .d.ts files
 * (ambient declaration files that do not have a .js counterpart).
 */
export function isTsSourcePath(filePath: string): boolean {
  // Exclude .d.ts — these are declaration-only files with no .js output.
  if (filePath.endsWith('.d.ts')) return false;
  return filePath.endsWith('.ts') || filePath.endsWith('.tsx');
}

/**
 * Strips the TypeScript extension from a file path and replaces it with `.js`.
 *
 * Examples:
 *   "src/app/page.ts"   → "src/app/page.js"
 *   "src/app/page.tsx"  → "src/app/page.js"
 *   "src/app/page.d.ts" → "src/app/page.d.js"  (caller must handle .d.ts)
 */
export function tsPathToJsPath(tsPath: string): string {
  return tsPath.replace(/\.tsx?$/, '.js');
}

export function jsPathToTsCandidates(jsPath: string): string[] {
  if (!/\.jsx?$/.test(jsPath)) return [];
  const stem = jsPath.replace(/\.jsx?$/, '');
  return [stem + '.ts', stem + '.tsx'];
}

// ---------------------------------------------------------------------------
// Resolver class
// ---------------------------------------------------------------------------

export class BazelResolver {
  readonly workspaceRoot: string;
  readonly bazelBin: string;
  readonly workspace: string | undefined;
  readonly mode: ResolverMode;
  private readonly realWorkspaceRoot: string;
  private readonly declaredFiles: NonNullable<ResolverOptions['declaredFiles']>;
  private readonly selectedFiles = new Map<string, string>();

  constructor(options: ResolverOptions) {
    this.workspaceRoot = options.workspaceRoot;
    this.realWorkspaceRoot = fs.existsSync(options.workspaceRoot)
      ? fs.realpathSync(options.workspaceRoot)
      : options.workspaceRoot;
    this.bazelBin = options.bazelBin;
    this.workspace = options.workspace;
    this.mode = options.mode ?? 'build';
    this.declaredFiles = this.mode === 'serve' ? options.declaredFiles ?? {} : {};
    for (const [logical, file] of Object.entries(this.declaredFiles)) {
      this.selectedFiles.set(file.path, logical);
    }
  }

  /**
   * Given an absolute path to a TypeScript source file, returns the absolute
   * path to the corresponding pre-compiled .js file under bazel-bin, along
   * with its source-map path if present.
   *
   * Returns null when the source file does not have a known bazel-bin
   * counterpart (e.g. the file is outside the workspace root).
   */
  resolveSourceToJs(absoluteTsPath: string): ResolvedFile | null {
    // Only handle .ts/.tsx source files.
    if (!isTsSourcePath(absoluteTsPath)) return null;

    // Compute the workspace-relative path of the source file.
    const rel = this.workspaceRelativePath(absoluteTsPath);

    // Bail out if the path escapes the workspace root (contains leading `..`).
    if (rel.startsWith('..')) return null;

    // Build the bazel-bin path by replacing the TypeScript extension with .js.
    const relJs = tsPathToJsPath(rel);
    const jsPath = path.join(this.bazelBin, relJs);

    return {
      jsPath,
      mapPath: this.findMapForJs(jsPath),
    };
  }

  /**
   * Given an absolute path to a .js file under bazel-bin, returns its
   * workspace-relative path (suitable for use as a Vite module ID).
   */
  jsPathToModuleId(absoluteJsPath: string): string | null {
    const rel = path.relative(this.bazelBin, absoluteJsPath);
    if (rel.startsWith('..')) return null;
    // Vite module IDs use forward slashes.
    return '/' + rel.split(path.sep).join('/');
  }

  /**
   * Given an absolute path to a .js file, returns the absolute path to the
   * companion .js.map file if it exists on disk, otherwise null.
   */
  findMapForJs(jsPath: string): string | null {
    const mapPath = jsPath + '.map';
    return fs.existsSync(mapPath) ? mapPath : null;
  }

  /** Bare specifiers return null in both modes: the launcher's node_modules
   *  link and `resolve.alias` are Vite's to resolve. */
  resolveId(
    id: string,
    importer?: string,
    extensions: readonly string[] = [],
    serveRoot?: string,
  ): Resolution | null {
    if (this.mode === 'build') {
      const built = this.resolveIdForBuild(id, importer);
      return built === null
        ? null
        : { filePath: built.jsPath, precompiled: true, mapPath: built.mapPath };
    }
    const cut = id.search(/[?#]/);
    const resolved = this.resolveIdForServe(cut < 0 ? id : id.slice(0, cut), importer, extensions, serveRoot);
    if (resolved === null || cut < 0) return resolved;
    const suffix = id.slice(cut);
    return {
      ...resolved,
      filePath: resolved.filePath + suffix,
      ...(resolved.directoryRequest === undefined ? {} : { directoryRequest: resolved.directoryRequest + suffix }),
    };
  }

  /**
   * Handles four cases:
   *  1. Absolute path pointing into the workspace source tree → redirect to
   *     its bazel-bin .js counterpart.
   *  2. Relative import whose importer lives in the workspace source tree →
   *     resolve relative to importer directory, then redirect.
   *  3. Relative import whose importer is already a bazel-bin .js file →
   *     resolve the import relative to both the source tree and bazel-bin,
   *     preferring the bazel-bin .js output when it exists.
   *  4. Anything else (bare specifier, non-ts absolute path, etc.) →
   *     return null to let Vite's default resolver take over.
   */
  private resolveIdForBuild(id: string, importer?: string): ResolvedFile | null {
    // ── Case 1: absolute path into source tree ────────────────────────────
    if (path.isAbsolute(id)) {
      return this.resolveSourceToJs(id);
    }

    // ── Cases 2 & 3: relative import ─────────────────────────────────────
    if (!isRelativeImport(id) || importer == null) return null;

    const importerDir = path.dirname(importer);
    const importerIsInBazelBin = importerDir.startsWith(this.bazelBin + path.sep)
      || importerDir === this.bazelBin;

    // ── Case 3: importer is a bazel-bin .js file ──────────────────────────
    // Relative .js-to-.js imports within bazel-bin are already resolved
    // correctly by Vite's default file-system resolver — we don't need to
    // intercept them.  But if the specifier has no extension (or a .ts
    // extension from source-authored code), we need to probe for the .js
    // output.
    if (importerIsInBazelBin) {
      const candidates = buildExtensionCandidates(id);
      for (const candidate of candidates) {
        const absInBazelBin = path.resolve(importerDir, candidate);
        // If the candidate path ends in .ts/.tsx, map it to its .js output.
        if (isTsSourcePath(absInBazelBin)) {
          // Derive the source-tree path from the bazel-bin path.
          const relFromBazelBin = path.relative(this.bazelBin, absInBazelBin);
          const sourceAbsolute = path.join(this.workspaceRoot, relFromBazelBin);
          const result = this.resolveSourceToJs(sourceAbsolute);
          if (result !== null && fs.existsSync(result.jsPath)) {
            return result;
          }
        } else if (absInBazelBin.endsWith('.js')) {
          // Already a .js path — only intercept if it lives under bazel-bin.
          if (absInBazelBin.startsWith(this.bazelBin + path.sep) && fs.existsSync(absInBazelBin)) {
            return { jsPath: absInBazelBin, mapPath: this.findMapForJs(absInBazelBin) };
          }
        }
      }
      return null;
    }

    // ── Case 2: importer is a source-tree .ts file ────────────────────────
    const candidates = buildExtensionCandidates(id);

    for (const candidate of candidates) {
      const absolute = path.resolve(importerDir, candidate);

      if (isTsSourcePath(absolute)) {
        const result = this.resolveSourceToJs(absolute);
        if (result !== null && fs.existsSync(result.jsPath)) {
          return result;
        }
      }
    }

    return null;
  }

  isDeclaredFile(file: string): boolean {
    return this.selectedCoordinate(file) !== undefined;
  }

  isBazelOutput(file: string): boolean {
    const realBin = this.currentRealPath(this.bazelBin);
    return [this.bazelBin, ...(realBin === undefined ? [] : [realBin])].some((root) => {
      const rel = path.relative(root, file);
      return rel !== '' && !rel.startsWith('..') && !path.isAbsolute(rel);
    });
  }

  isDeclaredOutput(file: string): boolean {
    const logical = this.selectedCoordinate(file);
    const selected = logical === undefined ? undefined : this.declaredFile(logical)?.file;
    return selected !== undefined && selected.isSource !== true;
  }

  declaredDirectory(file: string): string | undefined {
    const logical = this.selectedCoordinate(file);
    const declared = logical === undefined ? undefined : this.declaredFile(logical)?.file;
    return declared?.directory ? declared.path : undefined;
  }

  sourcePackage(file: string): { importer: string; manifest: string } | undefined {
    if (this.mode !== 'serve') return undefined;
    const logical = this.selectedCoordinate(file);
    const selected = logical === undefined ? undefined : this.declaredFile(logical)?.file;
    if (selected?.context !== 'source' || selected.directory || selected.scope === undefined) {
      return undefined;
    }
    return { importer: selected.importer ?? selected.path, manifest: selected.scope };
  }

  private selectedCoordinate(file: string): string | undefined {
    let clean = path.normalize(file.replace(/[?#].*$/, ''));
    if (clean.length > 1 && clean.endsWith(path.sep)) clean = clean.slice(0, -1);
    const selected = this.selectedLogicalPath(clean);
    if (selected !== undefined) return selected;
    if (this.selectedFiles.size === 0) return undefined;

    for (const root of [this.bazelBin, this.workspaceRoot]) {
      const realRoot = this.currentRealPath(root);
      if (realRoot === undefined) continue;
      const relative = path.relative(realRoot, clean);
      if (relative === '' || relative.startsWith('..') || path.isAbsolute(relative)) continue;
      const candidate = path.join(root, relative);
      const logical = this.selectedLogicalPath(candidate);
      if (logical !== undefined && this.currentRealPath(candidate) === clean) return logical;
    }

    // A declared file matches only a canonical query: a watcher event under the bazel-bin symlink must not
    // realpath every declared file, which kept oj's plugin host busy for minutes on a large application.
    let canonicalQuery = true;
    try {
      canonicalQuery = fs.realpathSync(clean) === clean;
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      canonicalQuery = code !== 'ENOENT' && code !== 'ENOTDIR';
    }
    for (const [declaredPath, logical] of this.selectedFiles) {
      if (!canonicalQuery && !this.declaredFiles[logical]!.directory) continue;
      const realPath = this.currentRealPath(declaredPath);
      if (realPath === clean) return logical;
      if (realPath !== undefined && this.declaredFiles[logical]!.directory
        && clean.startsWith(realPath + path.sep)) {
        return path.posix.join(logical, path.relative(realPath, clean).split(path.sep).join('/'));
      }
    }
    return undefined;
  }

  private selectedLogicalPath(file: string): string | undefined {
    for (let ancestor = file; ; ancestor = path.dirname(ancestor)) {
      const logical = this.selectedFiles.get(ancestor);
      if (logical !== undefined && (ancestor === file || this.declaredFiles[logical]!.directory)) {
        return path.posix.join(logical, path.relative(ancestor, file).split(path.sep).join('/'));
      }
      if (path.dirname(ancestor) === ancestor) return undefined;
    }
  }

  private currentRealPath(file: string): string | undefined {
    try {
      return fs.realpathSync(file);
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if (code !== 'ENOENT' && code !== 'ENOTDIR') throw error;
    }
    return undefined;
  }

  private declaredFile(logical: string): { file: DeclaredFile; suffix: string } | undefined {
    for (let ancestor = logical; ; ancestor = path.posix.dirname(ancestor)) {
      const file = Object.hasOwn(this.declaredFiles, ancestor) ? this.declaredFiles[ancestor] : undefined;
      if (file !== undefined && (ancestor === logical || file.directory)) {
        return { file, suffix: path.posix.relative(ancestor, logical) };
      }
      if (path.posix.dirname(ancestor) === ancestor) return undefined;
    }
  }

  private declaredResolution(logical: string): Resolution | null {
    const selected = this.declaredFile(logical);
    if (selected === undefined) return null;
    const { file, suffix } = selected;
    const filePath = file.directory && suffix === ''
      ? file.path + path.sep
      : path.join(file.path, suffix);
    const precompiled = file.context === 'source' && filePath.endsWith('.js') && this.isBazelOutput(filePath);
    return {
      filePath,
      precompiled,
      mapPath: precompiled ? this.findMapForJs(filePath) : null,
    };
  }

  private resolveIdForServe(
    id: string,
    importer: string | undefined,
    extensions: readonly string[],
    serveRoot: string | undefined,
  ): Resolution | null {
    let absolute: string;
    if (path.isAbsolute(id)) {
      const selected = this.selectedCoordinate(id);
      if (selected !== undefined) return this.declaredResolution(selected);
      absolute = this.sourcePathForBinPath(id) ?? id;
      if (serveRoot !== undefined && !this.underWorkspace(absolute)) {
        absolute = path.join(serveRoot, id);
      }
    } else {
      if (!isRelativeImport(id) || importer == null) return null;

      const selected = this.selectedCoordinate(importer);
      if (selected !== undefined && this.declaredFile(selected)!.file.context === 'asset') return null;

      const importerDir = path.dirname(importer);
      const resolveFrom = selected !== undefined
        ? path.dirname(path.join(this.workspaceRoot, selected))
        : this.sourcePathForBinPath(importerDir) ?? importerDir;
      absolute = path.resolve(resolveFrom, id);
    }
    const logical = this.workspaceRelativePath(absolute).split(path.sep).join('/');
    if (this.underWorkspace(absolute) && this.declaredFile(logical)?.file.directory) {
      return this.declaredResolution(logical);
    }

    const result = this.classify(
      [absolute, ...extensions.map((extension) => absolute + extension)],
      absolute,
    );
    const directoryRequest = result?.directoryRequest;
    if (result !== null && directoryRequest === undefined) return result;
    const index = this.classify(extensions.map((extension) => path.join(absolute, 'index' + extension)));
    if (result === null || directoryRequest === undefined) return index;
    return index === null ? result : { ...index, directoryRequest };
  }

  private classify(sourcePaths: readonly string[], request?: string): Resolution | null {
    const tsCandidates = request === undefined ? [] : jsPathToTsCandidates(request);
    for (const sourcePath of sourcePaths) {
      let asset: string | undefined;
      const aliases = sourcePath === request ? tsCandidates : [];
      for (const candidate of [...aliases, sourcePath]) {
        const logical = this.selectedLogicalPath(candidate) ?? (this.underWorkspace(candidate)
          ? this.workspaceRelativePath(candidate).split(path.sep).join('/')
          : this.selectedCoordinate(candidate));
        if (logical === undefined) continue;
        const declared = this.declaredFile(logical);
        if (declared === undefined) continue;
        if (declared.file.context === 'source') return this.declaredResolution(logical);
        if (asset === undefined || candidate === sourcePath) asset = logical;
      }
      if (asset !== undefined) return this.declaredResolution(asset);
    }

    for (const sourcePath of sourcePaths) {
      if (!this.underWorkspace(sourcePath)) continue;
      const aliases = sourcePath === request ? tsCandidates : [];
      for (const candidate of aliases) {
        if (fs.existsSync(candidate)) {
          return { filePath: candidate, precompiled: false, mapPath: null };
        }
      }

      const stat = fs.statSync(sourcePath, { throwIfNoEntry: false });
      if (stat !== undefined) {
        if (stat.isDirectory()) {
          if (sourcePath === request) {
            return { filePath: sourcePath, precompiled: false, mapPath: null, directoryRequest: sourcePath };
          }
          continue;
        }
        if (!isTsSourcePath(sourcePath)) return null;
        return { filePath: sourcePath, precompiled: false, mapPath: null };
      }

      const generatedPath = this.binPathForSourcePath(sourcePath);
      if (generatedPath === null) continue;
      for (const candidate of this.generatedCandidates(generatedPath)) {
        if (!fs.existsSync(candidate) || !fs.statSync(candidate).isFile()) continue;
        const precompiled = candidate.endsWith('.js');
        return {
          filePath: candidate,
          precompiled,
          mapPath: precompiled ? this.findMapForJs(candidate) : null,
        };
      }
    }
    return null;
  }

  private generatedCandidates(generatedPath: string): string[] {
    if (!isTsSourcePath(generatedPath)) return [generatedPath];
    return [generatedPath, tsPathToJsPath(generatedPath)];
  }

  /** Same workspace-relative path, rooted at bazel-bin instead. */
  private binPathForSourcePath(sourcePath: string): string | null {
    const rel = this.workspaceRelativePath(sourcePath);
    if (rel === '' || rel.startsWith('..') || path.isAbsolute(rel)) return null;
    return path.join(this.bazelBin, rel);
  }

  /** Inverse of binPathForSourcePath; null when the path is not under bazel-bin. */
  private sourcePathForBinPath(binPath: string): string | null {
    const rel = path.relative(this.bazelBin, binPath);
    if (rel === '' || rel.startsWith('..') || path.isAbsolute(rel)) return null;
    return path.join(this.workspaceRoot, rel);
  }

  private underWorkspace(absolute: string): boolean {
    const rel = this.workspaceRelativePath(absolute);
    return rel !== '' && !rel.startsWith('..') && !path.isAbsolute(rel);
  }

  private workspaceRelativePath(absolute: string): string {
    const rel = path.relative(this.workspaceRoot, absolute);
    if (!rel.startsWith('..') && !path.isAbsolute(rel)) return rel;
    // Vite canonicalizes importers, including Darwin's /var -> /private/var.
    return path.relative(this.realWorkspaceRoot, absolute);
  }
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

/**
 * Given a raw import specifier, returns a list of candidate paths to probe
 * when searching for the backing source file.  The specifier may or may not
 * carry an explicit extension.
 */
function buildExtensionCandidates(specifier: string): string[] {
  const hasExplicitExtension = /\.[a-z]+$/i.test(specifier);

  if (hasExplicitExtension) {
    // Already has an extension — only try as-is.
    return [specifier];
  }

  return [
    specifier + '.ts',
    specifier + '.tsx',
    specifier + '/index.ts',
    specifier + '/index.tsx',
  ];
}

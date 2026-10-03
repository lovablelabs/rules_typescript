"""The generated vitest config: the entry file the launcher hands vitest.

Layers, lowest precedence first: Bazel, the user's `config`,
`coverage_provider`, the snapshot layer; docs/rules/ts-test.md § The Generated
vitest Config.
"""

load("//tools/launcher:launcher.bzl", "rlocation_path", "runfiles_link_path", "runfiles_root_path")
load("//ts/private:providers.bzl", "TsConfigInfo")
load("//ts/private:toolchain.bzl", "get_tools_toolchain")

_SNAPSHOT_HELPERS = """\
const snapshotBase = (testPath) => {
  const p = testPath.split(sep).join('/');
  for (const [compiled, base] of Object.entries(SNAPSHOT_BASES)) {
    if (p === compiled || p.endsWith('/' + compiled)) return base;
  }
  return null;
};

// Vitest's default follows the runtime file; Bazel snapshots belong to the source.
const vitestDefaultSnapshotPath = (testPath, ext) =>
  join(dirname(testPath), '__snapshots__', basename(testPath) + ext);

let RUNFILES_MANIFEST;
const rlocation = (p) => {
  const dir = process.env.RUNFILES_DIR;
  if (dir) return resolve(dir, p);
  if (RUNFILES_MANIFEST === undefined) {
    const manifest = process.env.RUNFILES_MANIFEST_FILE;
    RUNFILES_MANIFEST = new Map(
      (manifest ? readFileSync(manifest, 'utf8').split('\\n') : [])
        .filter((line) => line.includes(' '))
        .map((line) => [line.slice(0, line.indexOf(' ')), \
line.slice(line.indexOf(' ') + 1)]),
    );
  }
  return RUNFILES_MANIFEST.get(p) ?? abs(p);
};
"""

_CONFIG_MERGE_HELPERS = """\
const isPlainObject = (v) =>
  v !== null && typeof v === 'object' && !Array.isArray(v) && \
!(v instanceof RegExp);

// Key-by-key merge with array concatenation, mirroring vite's mergeConfig.
const merge = (a, b) => {
  if (b === undefined) return a;
  if (!isPlainObject(a) || !isPlainObject(b)) return b;
  const out = { ...a };
  for (const [k, v] of Object.entries(b)) {
    if (Array.isArray(out[k]) && Array.isArray(v)) out[k] = [...out[k], ...v];
    else out[k] = merge(out[k], v);
  }
  return out;
};
"""

_SETUP_HELPERS = """\
// A setup entry or an import names a source; the program vitest runs is the
// compiled one (docs/rules/ts-test.md § Files at Run Time).
const COMPILED_EXT = {
  '.ts': ['.js'], '.tsx': ['.js', '.jsx'], '.mts': ['.mjs'], '.cts': ['.cjs'],
};
const compiledForms = (spec) => {
  const m = typeof spec === 'string' ? /\\.[cm]?tsx?$/.exec(spec) : null;
  if (!m || !(m[0] in COMPILED_EXT)) return [];
  return COMPILED_EXT[m[0]].map((ext) => spec.slice(0, -m[0].length) + ext);
};
const compiledSibling = (dir, spec) =>
  compiledForms(spec).find((c) => existsSync(resolve(dir, c))) ?? spec;
const moduleReferences = MODULE_REFERENCES.map(([logical, source, runtime, selected, extension]) => ({
  logical: resolve(WORKSPACE_DIR, logical),
  source: resolve(RUNFILES_ROOT, source),
  sourceRoot: resolve(RUNFILES_ROOT, source.split('/')[0]),
  runtime: resolve(RUNFILES_ROOT, runtime),
  selected: resolve(RUNFILES_ROOT, selected),
  extension,
}));
const runtimeModulePaths = new Map();
const exactModuleReferences = new Map();
const moduleLookups = new Map();
for (const entry of moduleReferences) {
  for (const path of [entry.runtime, entry.selected]) {
    const previous = runtimeModulePaths.get(path);
    runtimeModulePaths.set(path, previous !== undefined && previous !== entry ? null : entry);
  }
  for (const [source, sourceRoot] of [[entry.logical, WORKSPACE_DIR], [entry.source, entry.sourceRoot]]) {
    exactModuleReferences.set(source, entry.runtime);
    const place = (target) => {
      const candidate = resolve(dirname(entry.runtime), relative(dirname(source), target));
      const previous = moduleLookups.get(target);
      moduleLookups.set(target, previous !== undefined && previous !== candidate ? null : candidate);
    };
    place(source.slice(0, -entry.extension.length));
    for (let directory = dirname(source); directory.startsWith(sourceRoot + '/') || directory === sourceRoot; directory = dirname(directory)) {
      place(directory);
      if (directory === sourceRoot) break;
    }
  }
}
const moduleReference = (target, allowLookup = false) => {
  const cut = target.search(/[?#]/);
  const file = cut < 0 ? target : target.slice(0, cut);
  const suffix = cut < 0 ? '' : target.slice(cut);
  if (/[?&](?:raw|url)(?:[=&]|$)/.test(suffix)) return null;
  let selected = exactModuleReferences.get(file);
  if (!selected && allowLookup) {
    selected = moduleLookups.get(file);
    const directory = moduleLookups.get(dirname(file));
    const placed = directory && resolve(directory, basename(file));

    // A named file absent beside its directory's modules is no reference, so a paths list tries its next candidate.
    if (!selected && placed && (!/\\.[cm]?[jt]sx?$/.test(placed) || existsSync(placed))) selected = placed;
  }
  return selected ? selected + suffix : null;
};
const isRuntimeModule = (resolved) => {
  const id = resolved?.id;
  if (!id) return false;
  const cut = id.search(/[?#]/);
  return runtimeModulePaths.has(cut < 0 ? id : id.slice(0, cut));
};
const resolveModuleReference = async (context, target, importer, opts, allowLookup = false) => {
  const projected = moduleReference(target, allowLookup);
  if (projected === null) return null;
  const resolved = await context.resolve(projected, importer, { ...opts, skipSelf: true });
  return isRuntimeModule(resolved) ? resolved : null;
};
// Relative or bare: a `.ts` subpath into a workspace member names a source
// the member's view holds as its compiled file.
const compiledImports = {
  name: 'rules_typescript:compiled-imports',
  enforce: 'pre',
  async resolveId(id, importer, opts) {
    if (!importer || /^[/\\0]/.test(id)) return null;
    for (const form of compiledForms(id)) {
      const resolved = await this.resolve(
        form, importer, { ...opts, skipSelf: true },
      );
      if (resolved) return resolved;
    }
    return null;
  },
};
const FILES_ROOT = process.env.TS_TEST_FILES_ROOT;
if (!FILES_ROOT) {
  throw new Error('rules_typescript: TS_TEST_FILES_ROOT is unset; the ' +
    'generated config runs under the ts_test launcher');
}
const DISCOVERY_ROOT = resolve(FILES_ROOT, DISCOVERY_PREFIX);
const withCompiledRun = (config, inheritedRoot = ROOT, defaultRoot = true) => {
  const root = resolve(inheritedRoot, config.root ?? '.');
  const dir = resolve(DISCOVERY_ROOT, relative(RUNFILES_ROOT, root));
  const includeRoot = defaultRoot && config.root == null ? DISCOVERY_ROOT : dir;
  const ancestorCount = relative(dir, includeRoot).split(sep).filter(Boolean).length;
  const explicit = (entry) => isAbsolute(entry) || /^\\.{1,2}(?:[\\\\/]|$)/.test(entry);
  const bareReferences = new Set(['setupFiles', 'globalSetup']
    .flatMap((key) => [config.test?.[key]].flat())
    .filter((entry) => typeof entry === 'string' && !explicit(entry))
    .map((entry) => resolve(root, entry))
    .filter((entry) => {
      const projected = moduleReference(entry);
      return projected !== null && projected !== entry;
    }));
  const rewrite = (entry) => explicit(entry)
    ? moduleReference(resolve(root, entry), true) ?? compiledSibling(root, entry)
    : entry;
  const test = {
    ...config.test,
    include: Array.from({ length: ancestorCount + 1 }, (_, level) =>
      INCLUDE.map((pattern) => '../'.repeat(level) + pattern)).flat(),
    dir,
  };
  for (const key of ['setupFiles', 'globalSetup']) {
    if (Array.isArray(test[key])) test[key] = test[key].map(rewrite);
    else if (typeof test[key] === 'string') test[key] = rewrite(test[key]);
  }
  const plugins = bareReferences.size ? [...(config.plugins ?? []), {
    name: 'rules_typescript:setup-references',
    enforce: 'pre',
    async resolveId(id, importer, opts) {
      return bareReferences.has(id) ? resolveModuleReference(this, id, importer, opts) : null;
    },
  }] : config.plugins;
  return { ...config, root, plugins, test };
};
"""

_PATHS_HELPERS = """\
// tsc took the exact key, else the longest matching prefix, and the first of
// its values that resolved; the run does the same over the compiled tree.
const tsconfigPaths = (dir, paths) => {
  const entries = Object.entries(paths).map(([key, values]) => {
    const star = key.indexOf('*');
    const exact = star < 0;
    return {
      exact,
      prefix: exact ? key : key.slice(0, star),
      suffix: exact ? '' : key.slice(star + 1),
      values,
    };
  });
  entries.sort((a, b) =>
    Number(b.exact) - Number(a.exact) || b.prefix.length - a.prefix.length);
  const match = (id) => {
    for (const e of entries) {
      if (e.exact) {
        if (id === e.prefix) return { e, captured: '' };
        continue;
      }
      const fits = id.length >= e.prefix.length + e.suffix.length;
      if (fits && id.startsWith(e.prefix) && id.endsWith(e.suffix)) {
        const end = id.length - e.suffix.length;
        return { e, captured: id.slice(e.prefix.length, end) };
      }
    }
    return null;
  };
  return {
    name: 'rules_typescript:tsconfig-paths',
    enforce: 'pre',
    async resolveId(id, importer, opts) {
      if (/^[./\\0]/.test(id)) return null;
      const m = match(id);
      if (!m) return null;
      for (const value of m.e.values) {
        const target = resolve(dir, value.replace('*', m.captured));
        const projected = await resolveModuleReference(this, target, importer, opts, true);
        if (projected) return projected;
        const sibling = compiledSibling(dir, target);
        if (/\\.[cm]?[jt]sx?$/.test(sibling) && !existsSync(resolve(dir, sibling))) continue;
        const resolved = await this.resolve(sibling, importer, { ...opts, skipSelf: true });
        if (resolved) {
          return isRuntimeModule(resolved)
            ? resolved
            : await resolveModuleReference(this, resolved.id, importer, opts) ?? resolved;
        }
      }
      return null;
    },
  };
};
"""

_IDS_HELPERS = """\
// A module's id is its runfiles path where the runfiles hold the file and its
// realpath otherwise: a package's imports resolve from its place in the tree.
const SOURCE_ROOT = (() => {
  if (!SOURCE_PROBE) return null;
  try {
    const real = realpathSync(resolve(WORKSPACE_DIR, SOURCE_PROBE));
    if (real.endsWith('/' + SOURCE_PROBE)) {
      return real.slice(0, -(SOURCE_PROBE.length + 1));
    }
  } catch {}
  return null;
})();
const BIN_DIR = (() => {
  if (!BIN_PROBE) return null;
  try {
    const real = realpathSync(resolve(WORKSPACE_DIR, BIN_PROBE));
    const m = /^(.*?\\/bazel-out\\/[^/]+\\/bin)\\//.exec(real);
    if (m) return m[1];
  } catch {}
  return null;
})();
const FS_ALLOW = BIN_DIR ? [WORKSPACE_DIR, BIN_DIR] : [WORKSPACE_DIR];
const runfilesPath = (file) => {
  if (isAbsolute(file) && existsSync(file)) file = realpathSync(file);
  const out = /^.*?\\/bazel-out\\/[^/]+\\/bin\\/(.*)$/.exec(file);
  if (out) {
    const ext = /^external\\/([^/]+)\\/(.*)$/.exec(out[1]);
    const rlocation = ext ? ext[1] + '/' + ext[2] : WORKSPACE + '/' + out[1];
    return resolve(RUNFILES_ROOT, OVERLAYS[rlocation] ?? rlocation);
  }
  if (SOURCE_ROOT && file.startsWith(SOURCE_ROOT + '/')) {
    return resolve(WORKSPACE_DIR, file.slice(SOURCE_ROOT.length + 1));
  }
  return null;
};
const moduleIds = {
  name: 'rules_typescript:module-ids',
  enforce: 'pre',
  async resolveId(id, importer, opts) {
    if (id.startsWith('\\0')) return null;
    const nested = { ...opts, skipSelf: true };
    const request = id.startsWith(DISCOVERY_ROOT + '/')
      ? moduleReference(id) ?? id
      : id;
    const importerFile = importer?.split(/[?#]/, 1)[0];
    const origin = importerFile && /^\\.{1,2}\\//.test(id)
      ? runtimeModulePaths.get(importerFile)
      : null;
    const projected = origin
      ? await resolveModuleReference(this, resolve(dirname(origin.source), id), importer, opts, true)
      : null;
    const resolved = projected ?? await this.resolve(request, importer, nested);
    if (resolved?.external) return resolved;
    const moduleId = resolved?.id ?? id;
    const cut = moduleId.search(/[?#]/);
    const file = cut < 0 ? moduleId : moduleId.slice(0, cut);
    const held = file.startsWith(RUNFILES_ROOT + '/');
    if (file.includes('/node_modules/')) return resolved;
    const requestFile = request.split(/[?#]/, 1)[0];
    const requestPath = importerFile && /^\\.{1,2}\\//.test(requestFile)
      ? resolve(dirname(importerFile), requestFile)
      : requestFile.startsWith('/') && !requestFile.startsWith(RUNFILES_ROOT + '/')
        ? resolve(this.environment.config.root, '.' + requestFile)
        : requestFile;
    const staged = held ? file
      : requestPath.startsWith(RUNFILES_ROOT + '/') && existsSync(requestPath) && realpathSync(requestPath) === file
        ? requestPath
        : runfilesPath(file);
    if (staged === null) return resolved;
    if (!existsSync(staged)) {
      this.error(
        `rules_typescript: "${id}" resolved to ${file}, which this test's ` +
          'runfiles do not hold; a src, dep or data entry has to stage it (a ' +
          'wrangler rules module is a src of the ts_compile that imports it).',
      );
    }
    if (!resolved) return null;
    const query = cut < 0 ? '' : moduleId.slice(cut);
    return { ...resolved, id: staged + query };
  },
};
"""

def _test_file_include(extensions):
    suffix = extensions[0] if len(extensions) == 1 else "{" + ",".join(extensions) + "}"
    return "**/*.{test,spec}." + suffix

def _relative_dir(from_dir, to_dir):
    """Relative path from one workspace directory to another, "." when equal."""
    a = [p for p in from_dir.split("/") if p]
    b = [p for p in to_dir.split("/") if p]
    shared = 0
    for i in range(min(len(a), len(b))):
        if a[i] != b[i]:
            break
        shared = i + 1
    return "/".join([".."] * (len(a) - shared) + b[shared:]) or "."

def _js(value):
    """Renders a Starlark value as a JS literal."""
    return json.encode(value)

def _relative_import(from_path, to_path):
    """Relative path from the directory holding from_path to to_path.

    Both arguments are runfiles-relative paths, so the result is valid from the
    generated config wherever the runfiles tree is materialised.
    """
    from_dir = from_path.split("/")[:-1]
    to_parts = to_path.split("/")
    shared = 0
    for i in range(min(len(from_dir), len(to_parts) - 1)):
        if from_dir[i] != to_parts[i]:
            break
        shared = i + 1
    parts = [".."] * (len(from_dir) - shared) + to_parts[shared:]
    joined = "/".join(parts)
    return joined if joined.startswith("..") else "./" + joined

def _snapshot_layer(snapshot_bases, snapshot_root):
    """The root-only layer that redirects vitest's .snap paths.

    `resolveSnapshotPath` is one of vitest's non-project options, so this layer
    is merged at the root and never into a `test.projects` entry.
    """
    if not snapshot_bases:
        return "const snapshotLayer = {};"
    return "\n".join([
        "const snapshotLayer = {",
        "  test: {",
        "    resolveSnapshotPath: (testPath, ext) => {",
        "      const base = snapshotBase(testPath);",
        "      if (base === null) " +
        "return vitestDefaultSnapshotPath(testPath, ext);",
        "      return rlocation({prefix} + base + ext);".format(
            prefix = _js(snapshot_root + "/"),
        ),
        "    },",
        "  },",
        "};",
    ])

def _member_pattern(package_name):
    """The regex literal that matches one package's files under node_modules."""
    escaped = "".join([
        ("\\" + c) if c in "./" else c
        for c in package_name.elems()
    ])
    return "/\\/node_modules\\/" + escaped + "\\//"

def _vitest_config_content(
        config_rf,
        user_config_rf,
        coverage_provider,
        snapshot_bases = {},
        snapshot_root = "",
        entry_extensions = [],
        bin_probe = "",
        tsconfig_paths_rf = None,
        root_rel = ".",
        workspace_rel = ".",
        inline_members = [],
        source_probe = "",
        overlays = {},
        module_references = [],
        discovery_root = "",
        discovery_files = {},
        workspace_name = ""):
    """Builds the entry config that layers Bazel's config under the user's."""
    path_imports = "basename, dirname, isAbsolute, relative, resolve, sep"
    if snapshot_bases:
        path_imports = "basename, dirname, isAbsolute, join, relative, resolve, sep"
    lines = [
        "// AUTO-GENERATED by rules_typescript ts_test. Do not edit.",
        "//",
        "// Layers, lowest precedence first: Bazel machinery, " +
        "the user's config,",
        "// then coverage_provider and the snapshot layer, root only.",
        "// Arrays concatenate; scalars are overridden.",
        "import { " + path_imports + " } from 'node:path';",
        "import { fileURLToPath } from 'node:url';",
        "import { existsSync, readFileSync, realpathSync } from 'node:fs';",
    ]
    if user_config_rf:
        lines.append("import userConfigExport from '{}';".format(
            _relative_import(config_rf, user_config_rf),
        ))
    lines += [
        "",
        "const HERE = dirname(fileURLToPath(import.meta.url));",
        "const abs = (p) => resolve(HERE, p);",
        "const ROOT = resolve(HERE, {});".format(_js(root_rel)),
        "const WORKSPACE_DIR = resolve(HERE, {});".format(_js(workspace_rel)),
        "const RUNFILES_ROOT = dirname(WORKSPACE_DIR);",
        "const DISCOVERY_PREFIX = {};".format(_js(discovery_root)),
        "const WORKSPACE = {};".format(_js(workspace_name)),
        "const OVERLAYS = {};".format(_js(overlays)),
        "const MODULE_REFERENCES = {};".format(_js(module_references)),
        "const SOURCE_PROBE = {};".format(_js(source_probe)),
        "const BIN_PROBE = {};".format(_js(bin_probe)),
        "const INCLUDE = {};".format(
            _js([_test_file_include(entry_extensions)]),
        ),
        "",
    ]
    if snapshot_bases:
        lines += [
            "const SNAPSHOT_BASES = {};".format(_js(snapshot_bases)),
            _SNAPSHOT_HELPERS,
        ]

    lines += [
        _CONFIG_MERGE_HELPERS,
        _SETUP_HELPERS,
    ]
    lines += [
        "exactModuleReferences.set(resolve(FILES_ROOT, {}), resolve(RUNFILES_ROOT, {}));".format(_js(path), _js(discovery_files[path]))
        for path in sorted(discovery_files)
    ]
    lines += [
        _PATHS_HELPERS,
        _IDS_HELPERS,
    ]
    if tsconfig_paths_rf:
        lines += [
            "const TSCONFIG_PATHS = JSON.parse(readFileSync(",
            "  resolve(HERE, {}), 'utf8'));".format(
                _js(_relative_import(config_rf, tsconfig_paths_rf)),
            ),
            "const PATHS_DIR = resolve(HERE, TSCONFIG_PATHS.dir);",
            "const pathsPlugins = Object.keys(TSCONFIG_PATHS.paths).length",
            "  ? [tsconfigPaths(PATHS_DIR, TSCONFIG_PATHS.paths)]",
            "  : [];",
        ]
    else:
        lines.append("const pathsPlugins = [];")

    lines += [
        # A file under test is a build output, so its realpath lies outside the
        # vite root -- which the coverage default drops before instrumenting.
        "const bazelLayer = {",
        # Vite's cache and the pool's deps optimizer write under the root
        # otherwise, which is the runfiles tree.
        "  ...(process.env.TEST_TMPDIR ? " +
        "{ cacheDir: resolve(process.env.TEST_TMPDIR, '.vite') } : {}),",
        "  plugins: [moduleIds, compiledImports, ...pathsPlugins],",
        # A DOM environment loads through Vite's server, which serves fs.allow
        # alone: the runfiles hold every runfiles id, bazel-bin every realpath.
        "  server: { fs: { allow: FS_ALLOW } },",
        # A workspace member's .js keeps its sources' extensionless relative
        # imports, which vite resolves and node's loader rejects: vite runs it.
        "  test: {{ {} }},".format(", ".join([
            "coverage: { allowExternal: true }",
            "server: {{ deps: {{ inline: [{}] }} }}".format(
                ", ".join([_member_pattern(name) for name in inline_members]),
            ),
        ])),
        "};",
    ]

    if coverage_provider:
        lines.append(
            ("const providerLayer = {{ test: {{ coverage: {{ provider: {} " +
             "}} }} }};").format(_js(coverage_provider)),
        )
    else:
        lines.append("const providerLayer = {};")

    lines.append(_snapshot_layer(snapshot_bases, snapshot_root))
    lines += [
        "",
        "export default async (env) => {",
    ]
    if user_config_rf:
        lines += [
            "  let user = typeof userConfigExport === 'function'",
            "    ? await userConfigExport(env)",
            "    : await userConfigExport;",
            "  // A config file that default-exports an array is a list of " +
            "vitest",
            "  // projects; vitest 4 removed test.workspace and throws on it.",
            "  if (Array.isArray(user)) user = { test: { projects: user } };",
            "  if (!isPlainObject(user)) user = {};",
        ]
    else:
        lines.append("  const user = {};")
    lines += [
        "  const merged = withCompiledRun(merge(" +
        "merge(merge(bazelLayer, user), providerLayer), snapshotLayer));",
        "  // Every project gets its own Vite server, so the Bazel layer " +
        "has to be",
        "  // applied to each project too; coverage and snapshots are the " +
        "root's.",
        "  const projects = merged.test && merged.test.projects;",
        "  if (Array.isArray(projects)) {",
        "    merged.test = {",
        "      ...merged.test,",
        "      projects: projects.map((p) =>",
        "        isPlainObject(p) ? withCompiledRun(merge(bazelLayer, p), merged.root, user.root == null)" +
        " : p,",
        "      ),",
        "    };",
        "  }",
        "  return merged;",
        "};",
        "",
    ]
    return "\n".join(lines)

def _snapshot_bases(ctx, discovery_entries):
    bases = {}
    for path, (source, runtime, selected) in discovery_entries.items():
        parent = source.short_path[:-(len(source.basename) + 1)] if "/" in source.short_path else ""
        base = "/".join([part for part in [parent, "__snapshots__", source.basename] if part])
        bases[path] = base
        for file in [runtime, selected]:
            bases[rlocation_path(ctx, file)] = base
            bases[file.short_path] = base
    return {path: bases[path] for _length, path in sorted([(-len(path), path) for path in bases])}

def tsconfig_paths_action(ctx):
    """Writes the `paths` of the `tsconfig` chain for the generated config.

    Returns the JSON file, or None without a `tsconfig`.
    """
    if not ctx.file.tsconfig:
        return None
    tsconfig_paths = ctx.actions.declare_file(
        "_{}.vitest/tsconfig_paths.json".format(getattr(ctx.attr, "public_name", ctx.label.name)),
    )
    chain = [ctx.file.tsconfig]
    if TsConfigInfo in ctx.attr.tsconfig:
        chain += ctx.attr.tsconfig[TsConfigInfo].deps_tsconfigs.to_list()
    ctx.actions.run(
        inputs = chain,
        outputs = [tsconfig_paths],
        executable = get_tools_toolchain(ctx).tsaction,
        arguments = [
            "paths",
            "-tsconfig=" + ctx.file.tsconfig.path,
            "-package=" + ctx.label.package,
            "-bin_dir=" + ctx.bin_dir.path,
            "-out=" + tsconfig_paths.path,
        ],
        mnemonic = "TsTestPaths",
        progress_message = "TsTestPaths %{label}",
    )
    return tsconfig_paths

# The source root, at run time, is where this file's realpath ends.
def _source_probe(source_sets):
    for f in depset(transitive = source_sets).to_list():
        if f.is_source:
            return f.short_path
    return ""

def _package_path(ctx, name):
    """The runfiles path of `name` in the test's package."""
    return "/".join(
        [p for p in [ctx.workspace_name, ctx.label.package, name] if p],
    )

def vitest_config_action(
        ctx,
        test_entry_points,
        source_sets,
        entry_extensions,
        discovery_root,
        discovery_entries,
        tsconfig_paths,
        inline_members,
        overlays,
        runtime_files = (),
        selected = {}):
    """Writes the entry config for `ctx`'s test.

    `entry_extensions` are the selected source extensions, the
    generated `test.include`'s; `tsconfig_paths` is the file
    tsconfig_paths_action wrote or None; `overlays`
    the runfiles symlinks, runfiles path to File, whose build outputs are held
    under another name than their own. Returns struct(config, entry,
    stage, symlinks, root_rel): the generated file; the runfiles path the
    launcher writes it to; every file the launcher writes into the package as a
    regular file, its destination keyed by the runfiles path it is read from;
    the private runfiles entries of `config` and `config_srcs`; and vite's root
    relative to the package.
    """
    if ctx.attr.config_srcs and not ctx.file.config:
        fail(("ts_test {}: config_srcs names the modules `config` imports; " +
              "there is no `config`.").format(ctx.label))
    name = getattr(ctx.attr, "public_name", ctx.label.name)
    vitest_config = ctx.actions.declare_file(
        "_{}.vitest/config.mjs".format(name),
    )
    entry = _package_path(ctx, "_{}_vitest.config.mjs".format(name))
    stage = {rlocation_path(ctx, vitest_config): entry}

    # At its own runfiles path a source may be the package's src, which the
    # regular file takes the place of: the launcher reads it from a private one.
    symlinks = {}
    private = "/".join(
        [p for p in [ctx.label.package, "_{}.vitest".format(name)] if p],
    )
    user_config_rf = None
    if ctx.file.config:
        for f in [ctx.file.config] + ctx.files.config_srcs:
            key = private + "/" + runfiles_link_path(f)
            symlinks[key] = f
            stage[runfiles_root_path(ctx, key)] = rlocation_path(ctx, f)
        user_config_rf = rlocation_path(ctx, ctx.file.config)
    paths_rf = None
    if tsconfig_paths:
        paths_rf = _package_path(ctx, "_{}_tsconfig_paths.json".format(name))
        stage[rlocation_path(ctx, tsconfig_paths)] = paths_rf

    # A `config` from an ancestor package roots vite there.
    root_rel = "."
    root_dir = ctx.label.package
    if ctx.file.config:
        root_dir = ctx.file.config.short_path.rpartition("/")[0]
        root_rel = _relative_dir(ctx.label.package, root_dir)
    bin_probe = test_entry_points[0].short_path if test_entry_points else ""
    module_references = {}
    for source, runtime in runtime_files:
        if source.is_directory or runtime.is_directory:
            continue
        coordinate = runfiles_link_path(source)
        reference = [coordinate, rlocation_path(ctx, source), rlocation_path(ctx, runtime), rlocation_path(ctx, selected.get(runtime, runtime)), "." + source.extension]
        previous = module_references.get(coordinate)
        if previous != None and previous != reference:
            fail("ts_test {}: config module '{}' has conflicting runtime owners. Did you mean to retain one compiler owner for this source?".format(ctx.label, coordinate))
        module_references[coordinate] = reference

    ctx.actions.write(
        output = vitest_config,
        content = _vitest_config_content(
            config_rf = entry,
            user_config_rf = user_config_rf,
            coverage_provider = ctx.attr.coverage_provider,
            snapshot_bases = _snapshot_bases(ctx, discovery_entries),
            snapshot_root = ctx.workspace_name,
            entry_extensions = entry_extensions,
            discovery_root = discovery_root,
            discovery_files = {
                path: rlocation_path(ctx, runtime)
                for path, (_source, runtime, _selected) in discovery_entries.items()
            },
            bin_probe = bin_probe,
            tsconfig_paths_rf = paths_rf,
            root_rel = root_rel,
            workspace_rel = _relative_dir(ctx.label.package, ""),
            inline_members = inline_members,
            source_probe = _source_probe(source_sets),
            module_references = module_references.values(),
            workspace_name = ctx.workspace_name,
            overlays = {
                rlocation_path(ctx, f): runfiles_root_path(ctx, link)
                for link, f in overlays.items()
                if not f.is_source
            },
        ),
    )
    return struct(
        config = vitest_config,
        entry = entry,
        stage = stage,
        symlinks = symlinks,
        root_rel = root_rel,
    )

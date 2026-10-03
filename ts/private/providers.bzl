"""Provider definitions for rules_typescript.

A `direct` field carries only what the target itself produces. A rule that just
forwards a dep's files -- ts_compile relative to a dep's data files, say --
leaves the direct field empty and puts the closure in the transitive one; a
consumer that wants everything reachable reads the transitive field.
"""

TsInfo = provider(
    doc = """What a dep gives a consumer: the files its program and runtime
stage, the npm packages its closure holds and the store files they reach.

ts_compile, ts_codegen and ts_binary return it over their outputs; an npm
package target returns one naming its closure in `npm_packages` and nothing by
path, since its files reach a consumer through the importer's links into the
store; a workspace member's hub view forwards the member's.
""",
    fields = {
        "js": "depset of File: the .js this target produces -- compiled " +
              "output and JavaScript srcs staged as-is.",
        "runtime_sources": "depset of File: directly published TypeScript runtime inputs.",
        "runtime_source_owners": "depset of string: target labels requiring emit=True for a built-output consumer, including workspace members.",
        "transitive_runtime_sources": "depset of File: source-mode runtime inputs in the dependency closure, using store artifacts for npm members.",
        "js_maps": "depset of File: the .js.map beside them.",
        "declarations": "depset of File: the .d.ts this target produces and " +
                        "the ones it passes through from srcs. A global one " +
                        "is in scope in a consumer only when the consumer's " +
                        "tsconfig `types` names it.",
        "data": "depset of File: non-program srcs and explicit package assets. " +
                "Assets and JSON modules share the compiler-owned logical layout.",
        "manifest": "File or None: the local package-root package.json from srcs or package_scopes " +
                    "as built: target paths follow published Files; source extensions remain unchanged with emit=False, otherwise " +
                    "every source-file target is rewritten to the " +
                    "emitted file, <name>.package.json. A dependent's " +
                    "program root lays it at the package's path and the " +
                    "member's store tree copies it there. The runtime scope projection " +
                    "or the imported JSON module is in `data`.",
        "sources": "depset of File: the srcs the program reads as its own " +
                   "-- original .ts, .tsx, JavaScript and declarations, independently of runtime placement. " +
                   "A ts_test under the same tsconfig checks them as its own program's.",
        "tsconfig": "File or None: the tsconfig.json the program's options " +
                    "come from, the `tsconfig` attribute's file. A ts_test " +
                    "under the same file checks this target's sources as " +
                    "its own program's.",
        "transitive_js": "depset of File: the .js of this target and its " +
                         "first-party deps.",
        "transitive_js_maps": "depset of File: their .js.map.",
        "transitive_data": "depset of File: the data files of this target " +
                           "and its first-party deps, what a compiled module " +
                           "reaches beside itself at run time.",
        "transitive_es_twins": "depset of (File, File): for a program tsgo " +
                               "emits, each .js of this target and its " +
                               "first-party deps paired with the ES module " +
                               "oxc emits from the same source; the vitest " +
                               "runner stages the second at the first's path.",
        "npm_packages": "depset of NpmPackageInfo: the npm closure of this " +
                        "target's deps, what the ownership manifest names " +
                        "and a runner checks its packages against. A " +
                        "package itself arrives through its NpmPackageInfo.",
        "npm_files": "depset of File: the store files this target's program " +
                     "and runtime reach -- the importer links of its direct " +
                     "npm deps and their @types twins, the member links its " +
                     "deps name, every store tree and edge link of their " +
                     "closures, the hoist links whose names the closure " +
                     "holds with the trees they enter, and its first-party " +
                     "deps' npm_files. An action stages this and nothing " +
                     "else of the store.",
        "owners": "depset of struct(label, files, declarations, type_inputs, importers) " +
                  "with optional declaration_files, canonical_links, asset_files, runtime_files, runtime_scopes, scope_manifest, replaced_scope and npm_bindings: one " +
                  "record per first-party target in the closure, this one " +
                  "first -- the label a deps list writes, the sources, " +
                  "declarations, data and manifest as built it stages, and " +
                  "its declarations, consumer type inputs and npm importer directories. The tsgo action names the owner " +
                  "of a listed file from `files`; a consumer's program " +
                  "reads every record's `type_inputs`; when it holds a dep " +
                  "as sources, only that dep's declarations are replaced. " +
                  "Declared package scopes retain their original File paths " +
                  "for published sources and passthrough declarations. " +
                  "Explicit type_inputs never become runtime data or roots; package_scopes " +
                  "also supply runtime data without module mappings. Their original Files " +
                  "remain compiler inputs when runtime placement changes. Optional runtime_scopes " +
                  "holds immutable (original scope File, runtime File) pairs, separate from module " +
                  "mappings. Consumers reuse a pair only while its runtime File is in transitive_data. " +
                  "Prior records may omit these pairs; ordinary runtime data passes through, but " +
                  "a new scope placement cannot reuse an unproven occupant at its destination. " +
                  "Optional runtime_files holds immutable (source File, runtime File) pairs " +
                  "constructed by this owner for runtime File identity and package-scope projection; " +
                  "an empty tuple asserts no runtime mappings. Prior records may omit " +
                  "the field: their published runtime Files and record are preserved, " +
                  "without inventing source/runtime mappings. " +
                  "Optional canonical_links holds exact (link File, canonical File) pairs for identity-preserving placement; module aliases retain their canonical owner context, while metadata-only scope aliases may be copied at their admitted coordinates. " +
                  "Optional asset_files holds immutable (original File, logical coordinate, published File) triples for unchanged-byte ordinary data. Coordinates are producer package-local paths, with external/<repository>/ prefixes for external producers. Only live transitive_data Files contribute views; duplicate aliases retain the same original File and conflicting origins at one coordinate fail. JSON modules and scope projections use runtime_files/runtime_scopes instead; an explicit data File may also be a compiler input. Older records may omit the field without inferred origins. " +
                  "Optional declaration_files holds exact (source File, declaration File) pairs from the producer; prior owners may omit them. " +
                  "Optional npm_bindings retains (package name, importer link File, store tree File) " +
                  "facts for runtime lookup placement; it describes declared lookup contexts, not " +
                  "module imports. Prior records may omit it. " +
                  "An npm package's declarations reach a " +
                  "consumer through `npm_files`, not through a record.",
    },
)

_EMPTY = depset()

def label_text(label):
    """The label as a deps list writes it, the main repo's @@ dropped."""
    text = str(label)
    return text[2:] if text.startswith("@@//") else text

def _or_direct(transitive, direct):
    return direct if transitive == None else transitive

def ts_info(
        js = _EMPTY,
        js_maps = _EMPTY,
        runtime_sources = _EMPTY,
        transitive_runtime_sources = None,
        declarations = _EMPTY,
        data = _EMPTY,
        manifest = None,
        sources = _EMPTY,
        tsconfig = None,
        transitive_js = None,
        transitive_js_maps = None,
        transitive_data = None,
        transitive_es_twins = _EMPTY,
        npm_packages = _EMPTY,
        npm_files = _EMPTY,
        label = None):
    """A TsInfo for a target without first-party deps: each closure it
    leaves unsaid is the direct set, and `label` makes it the one owner."""
    owners = _EMPTY
    if label:
        # Generated trees publish a canonical subtree; analysis cannot enumerate their modules.
        runtime_files = depset(
            [file for file in data.to_list() if file.extension == "json"],
            transitive = [js, runtime_sources],
        ).to_list()
        owners = depset([struct(
            label = label_text(label),
            files = depset(transitive = [sources, declarations, data]),
            declarations = declarations,
            type_inputs = declarations,
            runtime_files = tuple([(file, file) for file in runtime_files]),
            importers = (),
        )])
    return TsInfo(
        js = js,
        js_maps = js_maps,
        runtime_sources = runtime_sources,
        runtime_source_owners = depset([label_text(label)] if label and runtime_sources else []),
        transitive_runtime_sources = _or_direct(transitive_runtime_sources, runtime_sources),
        declarations = declarations,
        data = data,
        manifest = manifest,
        sources = sources,
        tsconfig = tsconfig,
        transitive_js = _or_direct(transitive_js, js),
        transitive_js_maps = _or_direct(transitive_js_maps, js_maps),
        transitive_data = _or_direct(transitive_data, data),
        transitive_es_twins = transitive_es_twins,
        npm_packages = npm_packages,
        npm_files = npm_files,
        owners = owners,
    )

TsTestRunnerInfo = provider(
    doc = """A test runner: the target ts_test hands its compiled tests to.

//ts/runners:vitest and //ts/runners:node_test are the two shipped; a rule in
another ruleset returning this provider is a third. ts_test compiles the tests
against the importer chain they run in, and the runner's `launch` turns them
into the launcher's config and the runfiles of one test.
""",
    fields = {
        "packages": "list of string: the npm packages the runner needs in " +
                    "the test's npm closure, `vitest` for the vitest " +
                    "runner; ts_test fails at analysis naming the one no dep " +
                    "provides.",
        "hook": "File: the one module the runner loads into node before the " +
                "tests -- the node:test runner's resolver, the vitest " +
                "runner's reads recorder.",
        "supports_source_inputs": "bool: the runner transforms TypeScript runtime inputs.",
        "es_modules": "bool: True when the runner runs the program as ES " +
                      "modules whatever its tsconfig's module -- vitest, " +
                      "which imports every file through vite's transform -- " +
                      "so ts_test emits its srcs as such and stages a dep's " +
                      "ES twins; False for node:test, which runs the " +
                      "package's format.",
        "launch": "function(ctx, test) -> struct: the runner's half of one " +
                  "test's analysis. `test` is the struct ts_test builds from " +
                  "the compile (entry_points, entry_extensions, " +
                  "test_files_list, chain, transitive_js, es_twins, placed, " +
                  "runtime_data_sets, runtime_sources, runtime_inputs, runtime_files, asset_files, canonical_links, package_sources, inline_members, " +
                  "runner); `chain` is " +
                  "struct(dirs, rlocations, npm_files): the chain's " +
                  "node_modules directories nearest first, as bin-dir paths " +
                  "and as runfiles paths, and the store files the test " +
                  "reaches; `placed` is an empty compatibility mapping for " +
                  "older launch callbacks. `runtime_files` holds live exact " +
                  "source/runtime File pairs; `runtime_inputs` selects the " +
                  "test entry pairs. The result " +
                  "carries `mode` and `section` (the launcher config's mode " +
                  "and that mode's section), `env`, `files`, `symlinks` and " +
                  "`transitive_files` for the runfiles, and `output_groups`. Optional `replacements` maps an omitted original File to its explicit replacement File; the runner must bind that original's path through `symlinks`, and final runfiles admission verifies the exact File.",
    },
)

TsConfigInfo = provider(
    doc = """A tsconfig.json, the files it extends, its jsx when preserve and
its module when tsgo emits it.

Starlark cannot read the file, so a ts_config target declares what a rule needs
from it before any action runs: the `extends` chain, every file of which becomes
an action input, and the two compiler options that name an output.
""",
    fields = {
        "tsconfig": "File: The tsconfig.json this target declares.",
        "deps_tsconfigs": "depset of File: Every file `tsconfig` extends, transitively.",
        "jsx": "string: \"preserve\" when the chain's effective jsx is " +
               "preserve, so a .tsx emits .jsx as under tsc; \"\" otherwise.",
        "module": "string: the chain's effective module when tsgo emits it " +
                  "-- commonjs, node16, node18, nodenext -- lowercased as " +
                  "tsgo prints it, so a ts_compile declares the ES twin of " +
                  "each .js for the vitest runner; \"\" for an ES kind or " +
                  "preserve.",
    },
)

NpmPackageInfo = provider(
    doc = "Provider for npm package targets.",
    fields = {
        "package_name": "string: npm package name (e.g., 'react').",
        "package_version": "string: npm package version.",
        "peer_id": "string: a filesystem-safe token naming the peer set this resolution was made against, empty for a package pnpm resolved only one way. Two snapshots can share name@version and differ only here, and they are two different dependency graphs, so anything keying a package by name and version alone merges them.",
        "package_dir": "File or None: The package.json file at the root of " +
                       "the extracted package. None on a pnpm workspace " +
                       "member, which was never extracted from a tarball: " +
                       "its compile writes the manifest as built.",
        "package_root": "string: exec-root-relative directory the files in `all_files` hang off -- where `package_dir` sits for an extracted tarball, the member's directory under bazel-bin for a workspace member.",
        "all_files": "depset of File: every file of this package " +
                     "(package.json, .js, .d.ts, other assets), the files " +
                     "its store tree copies; a member's are its outputs, " +
                     "the manifest as built in place of the src.",
        "transitive_deps": "depset of NpmPackageInfo: Transitive npm dependencies.",
        "store": "NpmStoreInfo: this resolution's store tree and the links " +
                 "beside it (npm/private/store.bzl), one per snapshot, in " +
                 "the lockfile's package.",
    },
)

DevServerInfo = provider(
    doc = """How to start one dev server implementation.

The default is source-built oj (`//oj:dev_server`). Vite (`//vite:dev_server`)
is optional. A custom implementation returns this same provider.

Two things differ between implementations and neither can be papered over.
A server shipping as an npm package has no `File` to point at -- its executable
is a path under the importer's `node_modules` directory, reached through the
package's link -- so it sets `server_in_tree` and leaves `server_binary` None.
A native binary is the other way round; exactly one of the two must be set.

`config_dialect` names the config the server is handed; only Vite's is
generated. A server need not read all of it, and a field it drops is not always
a field it does without: one taking the serve root from argv instead says so in
`argv`, and one ignoring a field says so in `ignored_config_fields`.
""",
    fields = {
        "server_binary": "File or None: the server executable, for a server " +
                         "that is a build artifact. None when the server " +
                         "ships as an npm package, in which case " +
                         "server_in_tree names it instead.",
        "server_in_tree": "string: the server executable's path under the " +
                          "importer's node_modules directory, for a server " +
                          "that ships as an npm package. Empty when " +
                          "server_binary is set.",
        "argv": "list of string: the command line after the executable. `{config}` expands to the generated config's path and `{root}` to the directory being served; a server taking either somewhere other than where the other one takes it says so here rather than in the launcher.",
        "config_dialect": "string: which config format this server is handed. Only \"vite\" is generated today; a server reading its own format declares its own dialect, and the generator has to learn it before that server can be selected.",
        "runs_in_js_runtime": "bool: True when the executable is JavaScript and the toolchain Node runs it, False for a native binary. A native server still gets the toolchain Node on PATH: one whose plugin host is a Node process is not a Node-free one.",
        "ignored_config_fields": "list of string: dotted config paths this server does not honour, e.g. [\"server.open\"]. A target whose configuration depends on one of these fails at analysis time naming the field and the server, rather than starting a server that quietly does something else.",
        "native_react_refresh": "bool: True when the server applies React Fast Refresh itself. `react_refresh = True` then fails rather than stacking @vitejs/plugin-react on top of a transform that already ran.",
        "runtime_deps": "depset of File: everything the server needs in " +
                        "runfiles beyond the generated config and the " +
                        "importer's node_modules.",
    },
)

NodeModulesInfo = provider(
    doc = """An importer's node_modules: the links a `node_modules` target
declares into the virtual store, one per npm package the importer declares,
the importer above it and the lockfile's hidden hoist
(docs/rules/node-modules.md).""",
    fields = {
        "label": "Label: the node_modules target's, the importer's package " +
                 "and what a message names.",
        "dir": "string: the importer's node_modules directory as a bin-dir " +
               "path, `bazel-out/<cfg>/bin/<package>/node_modules`: the " +
               "parent of every link, which no artifact names.",
        "links": "dict of string -> NpmLinkInfo: per package name, the " +
                 "declared symlink `node_modules/<name>` and the store it " +
                 "enters.",
        "parent": "NodeModulesInfo or None: the importer above's.",
        "hoist": "NpmHoistInfo: the lockfile's hidden hoist, the root " +
                 "importer's `hoist` target's, the same on every importer " +
                 "of its chain.",
    },
)

NpmHoistInfo = provider(
    doc = """A lockfile's hidden hoist, what its `npm_store_hoist` target
returns: the root importer names the target in `hoist`, and every importer
on the chain carries it as `NodeModulesInfo.hoist`.""",
    fields = {
        "links": "dict of string -> NpmLinkInfo: per hoisted name, the " +
                 "declared symlink `node_modules/.pnpm/node_modules/<name>` " +
                 "(a `public-hoist-pattern` match's at the root importer's " +
                 "`node_modules/<name>`) and the store it enters.",
        "members": "dict of string -> File: per hoisted workspace member, " +
                   "the declared symlink alone, with no dependency on the " +
                   "member's tree; a consumer's closure holds the tree " +
                   "through the member's link target.",
    },
)

NpmLinkInfo = provider(
    doc = """One link `node_modules/<name>` into a store tree: an entry of
`NodeModulesInfo.links`, and what a `node_modules_member` target returns for
a workspace member beside the member's own TsInfo and NpmPackageInfo.""",
    fields = {
        "link": "File: the declared symlink `node_modules/<name>`.",
        "store": "NpmStoreInfo: the store the link enters.",
    },
)

BundlerInfo = provider(
    doc = """Information about a JavaScript bundler.

BundlerInfo is the interface behind ts_binary's `bundler` attr. The ruleset
ships no implementation; a consumer writes a rule that returns this provider
and names it there.

Two invocation modes are supported:

Mode 1 — Standard CLI (use_generated_config = False, the default):
  The bundler binary is invoked with:
    --entry <path_to_entry.js>
    --out-dir <output_dir>
    --format esm|cjs|iife
    --external <pkg>         (may be repeated)
    --sourcemap              (flag, no value)
    --config <config_file>   (optional)

Mode 2 — Generated config (use_generated_config = True):
  ts_binary generates a Vite lib-mode vite.config.mjs and invokes the bundler
  with four exec-root-relative arguments: that config, the entry .js, the
  output directory and the stylesheet to create. The binary runs Vite over the
  config with EXEC_ROOT, VITE_ENTRY_PATH and VITE_OUT_DIR set; see
  ts/private/bundle_action.bzl for the outputs it must produce.
""",
    fields = {
        "bundler_binary": "File or FilesToRunProvider: The bundler CLI executable. Use files_to_run for tools with runfiles.",
        "config_file": "File or None: Optional static bundler config file passed via --config (mode 1 only).",
        "runtime_deps": "depset of File: Additional files needed by the bundler at runtime.",
        "use_generated_config": "bool: When True, ts_binary generates a vite.config.mjs and invokes bundler_binary in mode 2. Default False.",
    },
)

def runtime_scope_destinations(scopes, source, runtime_path):
    source_dir = source.short_path.split("/")[:-1]
    runtime_dir = runtime_path.split("/")[:-1]
    nearest = {}
    depth = -1
    for scope in scopes:
        if source.owner.workspace_root != scope.owner.workspace_root:
            continue
        scope_dir = scope.short_path.split("/")[:-1]
        if source_dir[:len(scope_dir)] != scope_dir:
            continue
        scope_depth = len(scope_dir)
        if scope_depth > depth:
            nearest = {}
            depth = scope_depth
        if scope_depth == depth:
            distance = len(source_dir) - scope_depth
            nearest[scope] = "/".join(runtime_dir[:len(runtime_dir) - distance] + [scope.basename]) if distance <= len(runtime_dir) else None
    return nearest

def is_javascript(file):
    return not file.is_directory and file.extension in ["js", "mjs", "cjs"]

def runtime_links(owners):
    links = {}
    for owner in owners:
        for link, canonical in getattr(owner, "canonical_links", ()):
            links.setdefault(link, {})[canonical] = True
    return links

def canonical_runtime_file(file, links):
    for _ in range(len(links) + 1):
        targets = links.get(file)
        if targets == None:
            return file
        if len(targets) != 1:
            fail("runtime alias '{}' has conflicting canonical Files. Did you mean to retain one producer identity?".format(file.path))
        file = targets.keys()[0]
    fail("runtime aliases contain a cycle at '{}'. Did you mean to link each publication to its producer's runtime File?".format(file.path))

def runtime_mappings(owners, live):
    aliases = runtime_links(owners)
    mappings = [
        (owner, [
            (source, runtime)
            for source, runtime in list(getattr(owner, "runtime_files", ())) + [
                (original, published)
                for original, _coordinate, published in getattr(owner, "asset_files", ())
                if is_javascript(published) and published not in aliases
            ]
            if runtime in live
        ])
        for owner in owners
    ]
    origins = {}
    for _owner, pairs in mappings:
        for source, runtime in pairs:
            previous = origins.setdefault(runtime, source)
            if previous != source:
                fail("runtime File '{}' has conflicting source Files '{}' and '{}'. Did you mean to retain one producing source identity?".format(runtime.path, previous.short_path, source.short_path))
    return mappings

def require_emitted(consumer, info, requirement, runtime_path = None, runtime_files = None):
    require_emitted_inputs(consumer, info, requirement)
    require_runtime_scopes(consumer, info, requirement, runtime_path = runtime_path, runtime_files = runtime_files)

def require_emitted_inputs(consumer, info, requirement):
    if info.transitive_runtime_sources:
        owners = info.runtime_source_owners.to_list()
        fail("{}: {} requires emitted JavaScript or declarations from {}. Set emit = True on those targets, or use a source-native consumer.".format(consumer, requirement, ", ".join(owners)))

def require_runtime_scopes(consumer, info, requirement, runtime_path = None, runtime_files = None, module_paths = None, owners = None, staged_scopes = {}):
    def placed_path(file):
        return runtime_path(file) if runtime_path != None else file.path

    available = runtime_files if runtime_files != None else {placed_path(file): file for file in info.transitive_data.to_list()}
    owners = info.owners.to_list() if owners == None else owners
    selected = module_paths
    if selected == None:
        javascript = {file: True for file in info.transitive_js.to_list()}
        live_modules = javascript | {file: True for file in info.transitive_data.to_list()}
        modules = {(source, runtime): True for _owner, pairs in runtime_mappings(owners, live_modules) for source, runtime in pairs if runtime in javascript or is_javascript(runtime)}
        selected = [(source, runtime, placed_path(runtime)) for source, runtime in modules]
    scopes = {}
    provenance = {}
    for owner in owners:
        for source, runtime in getattr(owner, "runtime_files", ()):
            if source.basename == "package.json":
                scopes[source] = True
                provenance.setdefault(runtime, {})[source] = True
        for source, runtime in getattr(owner, "runtime_scopes", ()):
            scopes[source] = True
            provenance.setdefault(runtime, {})[source] = True
    for source, runtime, path in selected:
        runtime_dir = path.split("/")[:-1]
        destinations = runtime_scope_destinations(scopes, source, path)
        nearest = None
        for depth in range(len(runtime_dir), -1, -1):
            parent = "/".join(runtime_dir[:depth])
            if depth and available.get(parent) != None:
                fail(("{}: {} cannot establish package scope for '{}' beneath opaque runtime directory '{}'. " +
                      "Did you mean to keep the directory outside this module's runtime path, or publish its module and scope as individual Files?").format(
                    consumer,
                    requirement,
                    runtime.short_path,
                    parent,
                ))
            candidate = "/".join(runtime_dir[:depth] + ["package.json"])
            if nearest == None and candidate in available:
                nearest = candidate
        if nearest != None and staged_scopes.get(nearest) in destinations:
            continue
        if not destinations and nearest != None:
            fail(("{}: {} module '{}' has no source package scope but acquires runtime manifest '{}'. " +
                  "Did you mean to remove that runtime manifest or declare the module's source scope explicitly?").format(
                consumer,
                requirement,
                source.short_path,
                nearest,
            ))
        for scope, destination in destinations.items():
            placed = available.get(destination)
            if placed == scope or (placed != None and provenance.get(placed, {}).keys() == [scope]):
                if nearest != destination:
                    fail(("{}: {} package scope '{}' at '{}' is shadowed by runtime manifest '{}' for '{}'. " +
                          "Did you mean to remove the closer runtime manifest or declare that module's source scope explicitly?").format(
                        consumer,
                        requirement,
                        scope.short_path,
                        destination,
                        nearest,
                        runtime.short_path,
                    ))
                continue
            fail(("{}: {} requires runtime package scope '{}' from {} at '{}' for '{}'. " +
                  "Did you mean to generate the scope's package too, or depend on a ts_compile " +
                  "there with that package.json in srcs? Compilation alone may retain the " +
                  "original metadata, but a runnable consumer needs the translated scope.").format(
                consumer,
                requirement,
                scope.short_path,
                scope.owner,
                destination if destination != None else "outside the execution root",
                runtime.short_path,
            ))

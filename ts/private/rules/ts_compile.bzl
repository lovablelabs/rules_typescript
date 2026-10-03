"""Core TypeScript compilation rule: oxc and tsgo over the tsconfig's options.

With emit=True, ts_compile transforms .ts/.tsx into .js + .js.map + .d.ts outputs
in one TsEmit action: oxc's transform for an ES-module program, tsgo's emit
for a CommonJS-shaped one (docs/rules/ts-compile.md § The Module Format). A
program tsgo emits, declared by its ts_config's `module`, gets a second TsEmit:
the ES twin of each .js under <name>.es/, which a vitest test runs in place of
the .js. A .tsx under jsx: preserve emits .jsx, the name tsc gives it, with its
JSX left for the bundler.

JavaScript sources (.js/.mjs/.cjs) join the type program unchanged;
`checkJs` in the tsconfig type-checks them.

The .d.ts are the compilation boundary: a dependent's program reads them and
nothing else of the target, so a change that leaves them byte-identical
recompiles no dependent. They are TsInfo.declarations and the `declarations`
output group, never a default output: TsgoDeclare emits them when a dependent
reads them or the group is requested, and a leaf runs the check alone. A
ts_test under the target's tsconfig is the one dependent that reads the
sources instead, one program with the compile (`package_program`), and a
ts_test's own program emits no declarations (`declarations = False`).

tsgo checks every program under --noEmit, TsgoCheck, a validation in the
_validation output group, against the importer chain `node_modules` names: a
direct npm dep is
the link of the nearest importer that declares it, its closure the store trees
and edge links that link reaches, a `@types/<name>` twin the chain links comes
with it, a member link target brings the member's tree, and every first-party
dep's npm files come along. tsgo walks up from the importing file for a bare
specifier and nothing above a source in the exec root is an output, so tsaction
runs it from a program root holding the action's source inputs at their paths
and the output tree whole, with each importer's node_modules at the importer's
directory, and the declarations, manifest and data of every first-party dep at
or above the target's package laid over that package's sources -- never its
JavaScript, which a program reads through the declarations -- the dep's
package.json as built at the package's path, so the tsconfig's own include
names the srcs and the deps' declarations and every import resolves as it does
over a pnpm install, the package's own name through the nearest manifest
included. The check runs during `bazel build` and blocks no dependent; a type
error fails the build.
Under --//ts:declarations=tsgo a second run of the same program, TsgoDeclare,
emits the .d.ts with the declaration shape on its command line, so the
written tsconfig carries none and the check runs no declaration transformer;
a chain that sets isolatedDeclarations keeps declaration on, which the option
requires, and its check reports an unannotated export as `tsc -p` does.
The linter the root module's ts.lint() names runs over the same sources as a
second validation action, TsLint. The emit reads the same program root when
tsgo emits the JavaScript.

The default emit=False retains source files for transforming runtimes and validation,
without JavaScript or declaration emission. Every compiler option is the
tsconfig's; the emit knobs are the build flags
//ts:declarations (tsgo|oxc),
//ts:source_map, //ts:declaration_map and //ts:lib_check. Each action is a
function under ts/private/actions/; compile_program declares the outputs, calls
them in order and builds the providers, for ts_compile and for the ts_test rule
over the same attributes.
"""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//ts/private:node_modules.bzl", "importer_chain", "importer_linking")
load(
    "//ts/private:providers.bzl",
    "NodeModulesInfo",
    "NpmLinkInfo",
    "NpmPackageInfo",
    "TsConfigInfo",
    "TsInfo",
    "canonical_runtime_file",
    "is_javascript",
    "label_text",
    "runtime_links",
    "runtime_mappings",
    "runtime_scope_destinations",
)
load("//ts/private:runtime.bzl", "JS_TOOL_TOOLCHAIN_TYPE")
load(
    "//ts/private:toolchain.bzl",
    "OXC_TOOLCHAIN_TYPE",
    "TOOLS_TOOLCHAIN_TYPE",
    "TSGO_TOOLCHAIN_TYPE",
    "get_oxc_toolchain",
    "get_tools_toolchain",
)
load("//ts/private/actions:emit.bzl", "emit_action")
load("//ts/private/actions:format.bzl", "FORMAT_ATTR", "FormatConfigInfo", "format_action")
load("//ts/private/actions:lint.bzl", "LintConfigInfo", "lint_action")
load("//ts/private/actions:manifest.bzl", "manifest_action", "runtime_scope_inputs")
load(
    "//ts/private/actions:tsconfig.bzl",
    "tsconfig_action",
    "write_baseline_tsconfig",
)
load(
    "//ts/private/actions:tsgo.bzl",
    "npm_hub_entry",
    "npm_hub_label",
    "ownership_manifest",
    "tsgo_check",
)

_TS_EXTENSIONS = ["ts", "tsx"]

_JS_EXTENSIONS = ["js", "mjs", "cjs"]

_UNSHAPED_TS_EXTENSIONS = ["mts", "cts"]

# tsc's own naming for the declaration it emits from a JavaScript source.
_JS_DECLARATION_EXTENSION = {
    "js": ".d.ts",
    "mjs": ".d.mts",
    "cjs": ".d.cts",
}

_DECLARATION_SUFFIXES = (".d.ts", ".d.mts", ".d.cts")

_INSTRUMENTED_EXTENSIONS = [
    "ts",
    "tsx",
    "mts",
    "cts",
    "js",
    "jsx",
    "mjs",
    "cjs",
]

def _is_dts_source(f):
    """Returns True if the file is a declaration file."""
    return f.basename.endswith(_DECLARATION_SUFFIXES)

def _package_relative_path(f, pkg):
    """The path of a src relative to the target's package, extension intact."""
    p = f.short_path
    if p.startswith("../"):
        # An external-repo file: ../<repo name>/<rest>.
        parts = p.split("/", 2)
        if len(parts) == 3:
            p = parts[2]
    if pkg and p.startswith(pkg + "/"):
        p = p[len(pkg) + 1:]
    return p

def _strip_ts_extension(p):
    for ext in (".tsx", ".ts"):
        if p.endswith(ext):
            return p[:-len(ext)]
    return p

def _runtime_file(ctx, src, rel, relocate, copies = None):
    if src.path == _bin_dir(ctx, ctx.label) + "/" + rel:
        return src
    if not relocate and (
        not src.is_source or
        src.owner.workspace_root != ctx.label.workspace_root or
        (ctx.label.package and rel == _package_relative_path(src, ""))
    ):
        return src
    staged = ctx.actions.declare_file(rel)
    if copies != None:
        copies.append((src, staged))
    else:
        ctx.actions.symlink(output = staged, target_file = src)
    return staged

def _runtime_scope_targets(scope, destination, module_pairs):
    targets = []
    source_prefix = scope.short_path[:-len(scope.basename)]
    runtime_prefix = destination[:-len(scope.basename)]
    for source, runtime in module_pairs:
        placed = runtime.path
        if runtime_scope_destinations([scope], source, placed).get(scope) == destination:
            targets.append({"source": "./" + source.short_path[len(source_prefix):], "runtime": "./" + placed[len(runtime_prefix):]})
    return targets

def _runtime_scope_data(ctx, scopes, module_scopes, direct_data, dependency_data, data_sources, incoming_scopes, module_pairs, preserve_sources, canonical_links):
    available = {file.path: file for file in direct_data + dependency_data}
    provenance = {}
    for source, runtime in incoming_scopes + [pair for pair in module_pairs if pair[0] in module_scopes]:
        provenance.setdefault(runtime, {})[source] = True
    published = {(scope, scope): True for scope in scopes if available.get(scope.path) == scope}
    for source, runtime in data_sources.items():
        origins = provenance.get(source, {source: True}).keys()
        for origin in origins:
            provenance.setdefault(runtime, {})[origin] = True
            if origin in scopes:
                published[(origin, runtime)] = True
    staged = []
    checks = []
    replaced = {}
    output_prefix = _bin_dir(ctx, ctx.label) + "/"
    candidates = list(scopes) + module_scopes
    destinations = {}
    for source, runtime in module_pairs:
        for scope, destination in runtime_scope_destinations(candidates, source, runtime.path).items():
            if preserve_sources and runtime == source and destination not in available:
                destination = runtime_scope_destinations([scope], source, _logical_path(runtime)).get(scope)
            if destination != None:
                destinations[(scope, destination)] = True
    placed_scopes = {scope: True for scope, _destination in destinations}
    for scope in scopes:
        if scope not in placed_scopes and scope.owner.package == ctx.label.package and scope.owner.workspace_root == ctx.label.workspace_root:
            destinations[(scope, output_prefix + _package_relative_path(scope, ctx.label.package))] = True
    for scope, destination in destinations:
        placed = available.get(destination)
        origins = provenance.get(placed, {})
        if placed != None and placed != scope and origins.keys() != [scope]:
            others = [source for source in origins if source != scope]
            occupant = "'{}' from {}".format(others[0].short_path, others[0].owner) if others else "opaque runtime File '{}' from {}".format(placed.short_path, placed.owner)
            fail(("ts_compile: runtime package scope '{}' from {} on {} needs '{}', " +
                  "already occupied by {}; matching contents are not established.").format(
                scope.short_path,
                scope.owner,
                ctx.label,
                destination,
                occupant,
            ))
        targets = _runtime_scope_targets(scope, destination, module_pairs)
        if placed == None:
            if scope not in scopes or not destination.startswith(output_prefix):
                continue
            rel = destination[len(output_prefix):]
            if any([target["source"] != target["runtime"] for target in targets]):
                placed = ctx.actions.declare_file(rel)
                manifest_action(ctx, scope, placed, runtime_targets = targets)
            else:
                placed = _runtime_file(ctx, scope, rel, True)
                if placed != scope:
                    canonical_links.append((placed, scope))
            available[destination] = placed
            provenance[placed] = {scope: True}
            published[(scope, placed)] = True
            staged.append(placed)
        else:
            checks.append(struct(source = scope, runtime = placed, targets = targets))
        if placed != scope and placed.short_path == scope.short_path:
            replaced[scope] = True
    return (
        [file for file in direct_data if file not in replaced] + staged,
        replaced,
        tuple([pair for pair in published if pair[1] not in replaced]),
        checks,
    )

def _record_label(record):
    text = getattr(record, "label", "")
    return text[2:] if text.startswith("@@//") else text

def _logical_path(file):
    path = file.short_path
    return "external/" + path[3:] if path.startswith("../") else path

def _common_root(root, paths):
    parts = root.split("/") if root else []
    for path in paths:
        other = path.split("/")[:-1]
        size = 0
        for left, right in zip(parts, other):
            if left != right:
                break
            size += 1
        parts = parts[:size]
    return "/".join(parts)

def _layout_path(root, logical):
    if root and not logical.startswith(root + "/"):
        fail("ts_compile: module '{}' is outside layout root '{}'; publish its source/output association.".format(logical, root))
    return logical[len(root) + 1:] if root else logical

def _module_path(source, output):
    logical = _logical_path(source)
    if source == output or source.is_directory or _is_dts_source(source):
        return logical
    suffix = "." + output.extension
    for declaration in _DECLARATION_SUFFIXES:
        if output.basename.endswith(declaration):
            suffix = declaration
            break
    return logical[:-(len(source.extension) + 1)] + suffix

def _program_layout(package, sources, declarations, dependency_pairs, assets, emit):
    dependency_moved = any([_module_path(source, output) != _logical_path(output) for source, output in dependency_pairs])
    root = _common_root(package, [_logical_path(source) for source in sources]) if emit or dependency_moved else package
    moved = root != package or dependency_moved
    if moved:
        root = _common_root(root, [_logical_path(source) for source, _output in dependency_pairs] + [_logical_path(file) for file in declarations] + assets)
    return struct(root = root, moved = moved)

def _source_root(file, relative):
    return file.path[:-len(relative) - 1] if file.path != relative else ""

def _classify_srcs(ctx):
    """Splits srcs into TypeScript, JavaScript, declaration and data sets."""
    compile_srcs = []
    js_srcs = []
    passthrough_dts = []
    data_srcs = []
    for f in ctx.files.srcs:
        if f.is_directory:
            fail(
                "ts_compile: '{}' on {} is a directory.\n".format(
                    f.short_path,
                    ctx.label,
                ) +
                "srcs declares one output per file at analysis time, and a " +
                "directory has no file list until its action has run.\nA " +
                "tree of already-compiled output -- ts_codegen(out_dir = " +
                "...) -- belongs in deps, where it is staged whole and " +
                "reached through the tsconfig's `paths`.",
            )
        if _is_dts_source(f):
            passthrough_dts.append(f)
        elif f.extension in _TS_EXTENSIONS:
            compile_srcs.append(f)
        elif f.extension in _JS_EXTENSIONS:
            js_srcs.append(f)
        elif f.extension == "jsx":
            fail(
                "ts_compile: '{}' on {} is a .jsx file. ".format(
                    f.short_path,
                    ctx.label,
                ) +
                "JavaScript is staged unchanged, and tsc would transform the " +
                "JSX in one under every jsx mode but preserve.\nRename it to " +
                ".tsx -- TypeScript accepts the JavaScript in it unchanged " +
                "-- or drop the JSX and call it .js.",
            )
        elif f.extension in _UNSHAPED_TS_EXTENSIONS:
            fail(
                "ts_compile: '{}' on {} is a .{} file, and the rule ".format(
                    f.short_path,
                    ctx.label,
                    f.extension,
                ) +
                "emits .js and .d.ts from .ts alone.\nRename it to .ts, or " +
                "leave it out of srcs.",
            )
        else:
            data_srcs.append(f)
    return compile_srcs, js_srcs, passthrough_dts, data_srcs

def _types_twin(name):
    if name.startswith("@types/"):
        return None
    if name.startswith("@"):
        return "@types/" + name[1:].replace("/", "__")
    return "@types/" + name

def _manifest_of(importer):
    return "/".join([p for p in [importer.label.package, "package.json"] if p])

def _bin_dir(ctx, label):
    return "/".join([
        p
        for p in [ctx.bin_dir.path, label.workspace_root, label.package]
        if p
    ])

# A dep at or above this package shares its directory with the sources: the
# program root lays the dep's declarations and data over them.
def _encloses(dep, target):
    if dep.workspace_root != target.workspace_root:
        return False
    return dep.package == "" or dep.package == target.package or \
           target.package.startswith(dep.package + "/")

def _same_tsconfig(ctx, info):
    return info.tsconfig != None and info.tsconfig == ctx.file.tsconfig

def _importer_chain(ctx, packages):
    if not ctx.attr.node_modules:
        if packages:
            fail(("{}: the closure holds npm packages ({}) and " +
                  "`node_modules` names no importer whose chain resolves " +
                  "them; set it to the `node_modules` target of the nearest " +
                  "lockfile importer at or above this package, as Gazelle " +
                  "writes it.").format(
                ctx.label,
                ", ".join(sorted([info.package_name for info in packages])),
            ))
        return []
    return importer_chain(ctx.attr.node_modules[NodeModulesInfo])

def _resolve_on_chains(ctx, chain, dep, member_links, source_chains):
    """The candidates linking dep, or None and the reason none supplies its store."""
    name = dep.info.package_name
    candidates = []
    for lookup_chain in [chain] + source_chains:
        candidate = importer_linking(lookup_chain, name, member_links)
        if candidate != None:
            candidates.append(candidate)
    if any([candidate.link.store.tree == dep.info.store.tree for candidate in candidates]):
        return candidates, None
    if not candidates:
        return None, (("{}: '{}' in deps is linked by no importer on the chain {}; " +
                       "declare it in {} and run `pnpm install " +
                       "--lockfile-only`.").format(
            ctx.label,
            name,
            " -> ".join([label_text(i.label) for i in chain]),
            _manifest_of(chain[0]),
        ))
    importer, link = candidates[0].importer, candidates[0].link
    return None, (("{}: '{}' in deps is {} ({}, store {}) and the importer {} links {} (store {}); " +
                   "no declared importer context supplies that store File. Did you mean to name {} in " +
                   "deps, or retain the source's supplying importer in source_node_modules?").format(
        ctx.label,
        name,
        dep.info.store.key,
        dep.label,
        label_text(dep.info.store.tree.owner),
        label_text(importer.label),
        link.store.key,
        label_text(link.store.tree.owner),
        npm_hub_label(dep.info, importer.label.package),
    ))

def _retain_npm_link(ctx, links, link):
    previous = links.get(link.link.path)
    if previous != None and previous.store.tree != link.store.tree:
        fail(("{}: npm lookup '{}' selects different store Files {} and {}. " +
              "Did you mean to use one resolution at that importer location?").format(
            ctx.label,
            link.link.short_path,
            label_text(previous.store.tree.owner),
            label_text(link.store.tree.owner),
        ))
    links[link.link.path] = link

def _with_types(selected, member_links):
    links = [selected]
    twin = _types_twin(selected.name) if selected.name in selected.importer.links else None
    companion = importer_linking(selected.chain, twin, member_links) if twin else None
    if companion != None:
        links.append(companion)
    return links

def _bare_view_message(ctx, name):
    return ("{}: '{}' in deps is the hub's view of a workspace member; name " +
            "the link target of the importer that links it instead, " +
            "`//<importer>:node_modules/{}`, as Gazelle writes it.").format(
        ctx.label,
        name,
        name,
    )

def _npm_closure(direct, dep_sets):
    seen = {}
    packages = []
    closure = depset(transitive = dep_sets, order = "postorder").to_list()
    for info in direct + closure:
        for reached in [info] + info.transitive_deps.to_list():
            if reached.store.tree not in seen:
                seen[reached.store.tree] = True
                packages.append(reached)
    return packages

def compile_program(
        ctx,
        es_modules = False,
        es_twins = False,
        package_program = False,
        declarations = True,
        package_data = []):
    """Registers the actions over ctx's srcs, deps, tsconfig and node_modules.

    The body of ts_compile and of ts_test: one attrs dict, one set of action
    functions. `es_modules` emits the program as ES modules whatever its
    tsconfig's module, the vitest runner's program; `es_twins` adds, to a
    program tsgo emits, the ES twin of each .js for the vitest tests that
    depend on it; `package_program` checks every dep under the target's
    tsconfig from its sources, one program with the package's compile, the
    ts_test rule's; `declarations = False` declares no .d.ts and registers
    no declaration emit, a test's program.
    """
    emit = ctx.attr.emit
    declarations = declarations and emit
    pkg = ctx.label.package

    compile_srcs, js_srcs, passthrough_dts, data_srcs = _classify_srcs(ctx)
    oxc = get_oxc_toolchain(ctx) if emit and compile_srcs else None

    dep_npm_package_sets = []
    dep_npm_file_sets = []
    transitive_js_sets = []
    runtime_source_sets = []
    transitive_js_map_sets = []
    transitive_data_sets = []
    transitive_es_twins_sets = []

    direct_npm_deps = []
    npm_links = {}
    direct_labels = []
    owner_sets = []
    held_as_sources = {}
    overlays = {}
    dep_manifests = []
    joined_source_sets = []

    # A dep reached through the store is in the program there; a copy of its
    # files at their exec paths would duplicate every module.
    for dep in ctx.attr.deps:
        info = dep[TsInfo]
        if info.transitive_runtime_sources:
            runtime_source_sets.append(
                dep[NpmPackageInfo].store.transitive if NpmPackageInfo in dep else info.transitive_runtime_sources,
            )
        dep_npm_package_sets.append(info.npm_packages)
        if NpmPackageInfo in dep:
            npm_info = dep[NpmPackageInfo]
            link = dep[NpmLinkInfo] if NpmLinkInfo in dep else None
            if npm_info.package_dir == None and link == None:
                fail(_bare_view_message(ctx, npm_info.package_name))
            direct_npm_deps.append(struct(label = dep.label, info = npm_info, link = link))
            continue
        if package_program and _same_tsconfig(ctx, info):
            joined_source_sets.append(info.sources)
            held_as_sources[label_text(dep.label)] = True
        transitive_js_sets.append(info.transitive_js)
        transitive_js_map_sets.append(info.transitive_js_maps)
        transitive_data_sets.append(info.transitive_data)
        transitive_es_twins_sets.append(info.transitive_es_twins)
        dep_npm_file_sets.append(info.npm_files)
        direct_labels.append(label_text(dep.label))
        owner_sets.append(info.owners)
        if _encloses(dep.label, ctx.label):
            directory = _bin_dir(ctx, dep.label)
            overlays[directory] = directory[len(ctx.bin_dir.path) + 1:]
            if info.manifest and label_text(dep.label) not in held_as_sources:
                dep_manifests.append(info.manifest)

    # A direct package resolves along the chain nearest first, pnpm's walk-up;
    # the @types twin an importer links beside it is the package's to declare.
    direct_npm_infos = [dep.info for dep in direct_npm_deps]
    packages = _npm_closure(direct_npm_infos, dep_npm_package_sets)
    chain = _importer_chain(ctx, packages)
    member_links = {}
    for dep in direct_npm_deps:
        if dep.link != None:
            _retain_npm_link(ctx, member_links, dep.link)
    source_chains = [importer_chain(target[NodeModulesInfo]) for target in ctx.attr.source_node_modules]
    selected_npm = []
    lookup_npm = []

    # Every unresolved dep is reported at once, so one wrong importer context names all it breaks.
    unresolved = []
    for dep in direct_npm_deps:
        candidates, reason = _resolve_on_chains(ctx, chain, dep, member_links, source_chains)
        if reason != None:
            unresolved.append(reason)
            continue
        for candidate in candidates:
            links = _with_types(candidate, member_links)

            # Omitting a nearer link would expose another context's ancestor as a fallback.
            lookup_npm.extend(links)
            if candidate.link.store.tree == dep.info.store.tree:
                selected_npm.extend(links)
    if unresolved:
        fail("\n".join(unresolved))
    for selected in lookup_npm:
        _retain_npm_link(ctx, npm_links, selected.link)
    own_importers = [importer.dir for importer in chain]
    source_importers = {
        selected.importer.dir: True
        for selected in lookup_npm
        if selected.importer.dir not in own_importers
    }
    for info in packages:
        hoist = chain[0].hoist
        if info.package_name in hoist.links:
            _retain_npm_link(ctx, npm_links, hoist.links[info.package_name])
        elif info.package_dir == None and info.package_name in hoist.members:
            _retain_npm_link(ctx, npm_links, NpmLinkInfo(
                link = hoist.members[info.package_name],
                store = info.store,
            ))
    npm_files = depset(
        [entry.link for entry in npm_links.values()],
        transitive = (
            [entry.store.transitive for entry in npm_links.values()] + dep_npm_file_sets
        ),
    )
    own_importers = source_importers.keys() + own_importers
    dependency_importers = {}
    dependency_owners = depset(transitive = owner_sets).to_list()
    dependency_data = depset(transitive = transitive_data_sets, order = "postorder").to_list()
    for record in dependency_owners:
        for directory in record.importers:
            if directory not in own_importers:
                dependency_importers[directory] = True
    own_bin_dir = _bin_dir(ctx, ctx.label)
    for record in dependency_owners:
        # A transitive publisher's as-built manifest is the only copy of the scope it replaced.
        scope_manifest = getattr(record, "scope_manifest", None)
        if scope_manifest == None or record.label in held_as_sources or scope_manifest in dep_manifests:
            continue
        if own_bin_dir == scope_manifest.dirname or own_bin_dir.startswith(scope_manifest.dirname + "/"):
            dep_manifests.append(scope_manifest)
    importers = [importer.dir for importer in chain]
    inherited_importers = dependency_importers.keys() + source_importers.keys()

    declared_trees = {selected.link.store.tree: True for selected in selected_npm}
    npm_reachable = [
        npm_hub_entry(info)
        for info in packages
        if info.store.tree not in declared_trees
    ]

    source_files = {src: True for src in ctx.files.srcs}
    scope_inputs = {file: True for file in ctx.files.package_scopes if file not in source_files}.keys()
    for file in scope_inputs:
        if file.is_directory or file.basename != "package.json":
            fail(("ts_compile: '{}' in package_scopes on {} is not a package.json File. " +
                  "Did you mean to put a JSON module in srcs or compiler-only metadata in type_inputs?").format(file.short_path, ctx.label))
    type_files = {file: True for file in ctx.files.type_inputs if file not in source_files}
    type_files.update({file: True for file in scope_inputs})
    type_inputs = type_files.keys()
    dep_type_sets = []
    own_compiler_paths = {_logical_path(file): file for file in source_files.keys() + type_inputs}
    for record in dependency_owners:
        dropped = {}
        replaced = getattr(record, "replaced_scope", None)

        # The as-built manifest, or a File at the scope's compiler path, stands in for the replaced scope.
        if replaced != None and (record.scope_manifest in dep_manifests or own_compiler_paths.get(_logical_path(replaced), replaced) != replaced):
            dropped[replaced] = True
        if record.label in held_as_sources:
            # Joined sources replace declarations, but still need their compiler metadata.
            dropped.update({file: True for file in record.declarations.to_list()})
        if dropped:
            dep_type_sets.append(depset([file for file in record.type_inputs.to_list() if file not in dropped], order = "postorder"))
        else:
            dep_type_sets.append(record.type_inputs)
    dep_dts_depset = depset(type_inputs, transitive = dep_type_sets, order = "postorder")

    # Previous owners publish modules through the runtime closure without source mappings.
    dependency_runtime = {file: True for file in depset(transitive = transitive_js_sets + runtime_source_sets).to_list()}
    live_modules = dependency_runtime | {file: True for file in dependency_data}
    dependency_pairs = [
        pair
        for record in dependency_owners
        for pair in getattr(record, "runtime_files", ())
        if pair[1] in live_modules
    ]
    compiler_type_files = {file: True for file in dep_dts_depset.to_list()}
    dependency_declarations = [
        pair
        for record in dependency_owners
        for pair in getattr(record, "declaration_files", ())
        if pair[1] in compiler_type_files
    ]
    data_files = {}
    for src in package_data:
        if src.is_directory:
            fail(("ts_compile: '{}' in data on {} is a directory. Did " +
                  "you mean to list individual asset files or put a " +
                  "ts_codegen tree in deps?").format(src.short_path, ctx.label))
        if src not in source_files:
            data_files[src] = True
    data_files.update({src: src.extension != "json" for src in data_srcs})
    links = runtime_links(dependency_owners)
    input_canonical = {file: canonical_runtime_file(file, links) for file in source_files.keys() + data_files.keys()}
    published_modules = {
        runtime: True
        for _owner, pairs in runtime_mappings(dependency_owners, live_modules | {file: True for file in input_canonical.values()})
        for _source, runtime in pairs
    }
    published_modules.update(dependency_runtime)
    known_publications = live_modules | published_modules

    known_publications.update({runtime: True for owner in dependency_owners for _source, runtime in getattr(owner, "runtime_scopes", ())})
    package = "/".join([part for part in [ctx.label.workspace_root, pkg] if part])
    data_coordinates = {
        src: _logical_path(src) if src in source_files and src.extension == "json" else "/".join([part for part in [package, _package_relative_path(src, pkg)] if part])
        for src in data_files
    }
    live_data = {file: True for file in dependency_data + input_canonical.values()}
    dependency_asset_files = [
        (original, coordinate, published)
        for record in dependency_owners
        for original, coordinate, published in getattr(record, "asset_files", ())
        if published in live_data
    ]
    dependency_assets = {(original, coordinate): True for original, coordinate, _published in dependency_asset_files}
    asset_publications = {}
    for original, coordinate, published in dependency_asset_files:
        asset_publications.setdefault(published, []).append((original, coordinate))
    known_publications.update(asset_publications)
    retained_data = {input_canonical[src]: True for src in data_files if input_canonical[src] in known_publications}
    transitive_data_sets.append(depset(retained_data.keys(), order = "postorder"))
    dependency_data = depset(dependency_data + retained_data.keys(), order = "postorder").to_list()
    asset_origins = {}
    own_assets = {
        src: coordinate
        for src, coordinate in data_coordinates.items()
        if src not in scope_inputs and not (src in source_files and src.extension == "json") and input_canonical[src] not in known_publications
    }
    for original, coordinate in dependency_assets.keys() + own_assets.items():
        previous = asset_origins.get(coordinate)
        if previous != None and previous != original:
            fail("ts_compile: asset coordinate '{}' is claimed by both '{}' and '{}'; use distinct package-local data paths.".format(coordinate, previous.path, original.path))
        asset_origins[coordinate] = original
    layout = _program_layout(package, compile_srcs + js_srcs + [file for file in data_srcs if file.extension == "json"], passthrough_dts, dependency_pairs + dependency_declarations, asset_origins.keys(), emit)

    # Starlark cannot read the file to follow its extends chain, so a ts_config
    # target declares it and every file in it is an action input.
    baseline_file = write_baseline_tsconfig(ctx)
    tsconfig_chain = [baseline_file]
    declared_jsx = ""
    declared_module = ""
    if ctx.file.tsconfig:
        tsconfig_chain.append(ctx.file.tsconfig)
        if TsConfigInfo in ctx.attr.tsconfig:
            config_info = ctx.attr.tsconfig[TsConfigInfo]
            tsconfig_chain += config_info.deps_tsconfigs.to_list()
            declared_jsx = config_info.jsx
            declared_module = config_info.module
    tsx_extension = ".jsx" if declared_jsx == "preserve" else ".js"

    declarations_flag = ctx.attr._declarations[BuildSettingInfo].value
    oxc_emits_dts = declarations and declarations_flag == "oxc"
    tsgo_emits_dts = declarations and declarations_flag == "tsgo"
    source_map = ctx.attr._source_map[BuildSettingInfo].value
    declaration_map = ctx.attr._declaration_map[BuildSettingInfo].value
    checkers = ctx.attr._checkers[BuildSettingInfo].value

    if declaration_map and declarations_flag == "oxc":
        fail(
            "ts_compile: --//ts:declaration_map needs the tsgo declaration " +
            "emit, and --//ts:declarations=oxc takes it away.\noxc writes " +
            "declarations syntactically and emits no map for them.\nBuild " +
            "with one flag or the other.",
        )

    out_base = _bin_dir(ctx, ctx.label)

    emit_roots = {}
    emit_outputs = []
    emitted = {}
    js_outputs = []
    js_map_outputs = []
    dts_outputs = []
    dts_roots = {}
    declaration_files = {}
    dts_map_outputs = []
    twin_pairs = []
    twins_dir = "{}.es".format(ctx.label.name)

    for src in compile_srcs if emit else []:
        relative = _layout_path(layout.root, _logical_path(src))
        stem = _strip_ts_extension(relative)
        emit_roots[_source_root(src, relative)] = True

        js_extension = tsx_extension if src.extension == "tsx" else ".js"
        js_out = ctx.actions.declare_file(stem + js_extension)
        js_outputs.append(js_out)
        emit_outputs.append(js_out)
        emitted[src] = [js_out]
        if es_twins and declared_module:
            twin_pairs.append((js_out, ctx.actions.declare_file(
                "{}/{}".format(twins_dir, stem + js_extension),
            )))
        if source_map:
            js_map_out = ctx.actions.declare_file(stem + js_extension + ".map")
            js_map_outputs.append(js_map_out)
            emit_outputs.append(js_map_out)
            emitted[src].append(js_map_out)
        if declarations:
            dts_out = ctx.actions.declare_file(stem + ".d.ts")
            dts_outputs.append(dts_out)
            declaration_files[src] = dts_out
            dts_roots[_source_root(src, relative)] = True
            if oxc_emits_dts:
                emit_outputs.append(dts_out)
            if declaration_map:
                dts_map_outputs.append(
                    ctx.actions.declare_file(stem + ".d.ts.map"),
                )

    # A .mjs beside its .d.mts leaves the program (tsc keeps the higher-priority
    # extension), so tsgo writes no declaration for it: checked_in_dts.
    checked_in_dts = {
        _logical_path(d): True
        for d in passthrough_dts
    }
    js_passthrough = []
    runtime_copies = []
    runtime_files = {}
    for src in js_srcs:
        rel = _layout_path(layout.root, _logical_path(src)) if emit or layout.moved else _package_relative_path(src, pkg)
        canonical = input_canonical[src]
        runtime = canonical
        if canonical not in known_publications:
            runtime = src if not emit and not layout.moved else _runtime_file(ctx, src, rel, layout.moved, runtime_copies)
        js_passthrough.append(runtime)
        if canonical not in known_publications:
            runtime_files[src] = runtime
        stem = rel[:-(len(src.extension) + 1)]
        dts_rel = stem + _JS_DECLARATION_EXTENSION[src.extension]
        if tsgo_emits_dts and _logical_path(src)[:-(len(src.extension) + 1)] + _JS_DECLARATION_EXTENSION[src.extension] not in checked_in_dts:
            dts_out = ctx.actions.declare_file(dts_rel)
            dts_outputs.append(dts_out)
            declaration_files[src] = dts_out
            dts_roots[_source_root(src, rel)] = True
            if declaration_map:
                dts_map_outputs.append(
                    ctx.actions.declare_file(dts_rel + ".map"),
                )

    declaration_srcs = declaration_files.keys()
    published_dts = []
    for source in passthrough_dts:
        declaration = _runtime_file(ctx, source, _layout_path(layout.root, _logical_path(source)), True) if layout.moved else source
        published_dts.append(declaration)
        declaration_files[source] = declaration
    declaration_links = []
    canonical_links = []

    data_staged = list(scope_inputs)
    data_sources = {}
    asset_files = []
    scope_origins = {}
    for record in dependency_owners:
        for source, runtime in getattr(record, "runtime_scopes", ()):
            scope_origins.setdefault(runtime, []).append(source)
    for src, stage in data_files.items():
        rel = _package_relative_path(src, pkg)
        if emit or layout.moved:
            coordinate = data_coordinates[src]
            origin_path = _logical_path(src)

            # A scope copied at its source's own path governs the modules beside that source, not an asset path.
            if src in scope_origins and origin_path in [_logical_path(origin) for origin in scope_origins[src]]:
                if not layout.root or origin_path.startswith(layout.root + "/"):
                    coordinate = origin_path
            rel = _layout_path(layout.root, coordinate)
        canonical = input_canonical[src]
        if src in scope_inputs:
            runtime = src
        elif canonical in known_publications:
            runtime = canonical
            if out_base + "/" + rel != canonical.path:
                runtime = ctx.actions.declare_file(rel)
                if canonical in published_modules:
                    ctx.actions.symlink(output = runtime, target_file = canonical)
                    canonical_links.append((runtime, canonical))
                else:
                    runtime_copies.append((canonical, runtime))
                asset_files.extend([(original, coordinate, runtime) for original, coordinate in asset_publications.get(canonical, [])])
        elif not emit and not layout.moved and src in source_files and src.extension == "json":
            runtime = src
        else:
            runtime = _runtime_file(ctx, src, rel, stage or layout.moved, runtime_copies)
        data_sources[src] = runtime
        if src in scope_inputs and runtime.short_path == src.short_path:
            data_staged.remove(src)
        data_staged.append(runtime)
        if src in source_files and src.extension == "json" and canonical not in known_publications:
            runtime_files[src] = runtime
        elif src in own_assets:
            asset_files.append((src, own_assets[src], runtime))
    manifest = None
    for src in data_srcs + scope_inputs:
        if src.owner.package == pkg and src.owner.workspace_root == ctx.label.workspace_root and _package_relative_path(src, pkg) == "package.json":
            if emit or layout.moved:
                manifest = ctx.actions.declare_file(
                    "{}.package.json".format(ctx.label.name),
                )
                manifest_source = src
            else:
                manifest = src

    # JavaScript srcs whose declarations are all checked in leave tsgo nothing
    # to write: a program to check, not one to emit from.
    tsgo_emits_dts = tsgo_emits_dts and bool(dts_outputs)

    runtime_sources = []
    for source in compile_srcs if not emit else []:
        canonical = input_canonical[source]
        runtime = canonical if canonical in known_publications else (_runtime_file(ctx, source, _layout_path(layout.root, _logical_path(source)), True, runtime_copies) if layout.moved else source)
        runtime_sources.append(runtime)
        if canonical not in known_publications:
            runtime_files[source] = runtime
    runtime_files.update({src: outputs[0] for src, outputs in emitted.items()})
    linked_modules = []

    # A dependency record holds a mapping only for a runtime File its own target produced (ts-compile.md, Sources).
    held_mappings = {
        (source, runtime): True
        for record in dependency_owners
        for source, runtime in getattr(record, "runtime_files", ())
        if label_text(runtime.owner) == _record_label(record)
    }
    if layout.moved:
        placement_links = runtime_links(dependency_owners + [struct(canonical_links = tuple(canonical_links))])
        occupied = {file.path: canonical_runtime_file(file, placement_links) for file in runtime_sources + js_outputs + js_passthrough + data_staged + dts_outputs + published_dts}
        runtime_pairs = dependency_pairs + [(src, input_canonical[src]) for src in js_srcs if input_canonical[src] in known_publications]
        for pairs, links, own_outputs, placed_modules in [(runtime_pairs, data_staged, runtime_files, linked_modules), (dependency_declarations, declaration_links, declaration_files, [])]:
            for source, canonical in pairs:
                if source in own_outputs and own_outputs[source] != canonical:
                    continue
                relative = _layout_path(layout.root, _module_path(source, canonical))
                destination = out_base + "/" + relative
                if destination == canonical.path:
                    continue
                previous = occupied.get(destination)
                if previous != None:
                    if previous != canonical:
                        fail("ts_compile: dependency '{}' needs '{}' already occupied by '{}'; separate the conflicting source owners.".format(canonical.path, destination, previous.path))
                    continue
                link = ctx.actions.declare_directory(relative) if canonical.is_directory else ctx.actions.declare_file(relative)
                ctx.actions.symlink(output = link, target_file = canonical)
                occupied[destination] = canonical
                links.append(link)
                canonical_links.append((link, canonical))
                if (source, canonical) in held_mappings:
                    placed_modules.append((source, link))
        placed_assets = {published.path: original for original, _coordinate, published in asset_files}
        available_assets = {}
        asset_modules = {runtime: True for _owner, pairs in runtime_mappings(dependency_owners, live_data) for _source, runtime in pairs}
        canonical_assets = {}
        for original, coordinate, published in dependency_asset_files:
            if published not in asset_modules:
                continue
            previous = canonical_assets.setdefault((original, coordinate), published)
            if previous != published:
                fail("ts_compile: executable asset '{}' at '{}' has distinct canonical Files '{}' and '{}'. Did you mean to depend on one owning producer?".format(original.short_path, coordinate, previous.path, published.path))
        for original, _coordinate, published in dependency_asset_files:
            previous = available_assets.get(published.path)
            if previous != None and previous != (original, published):
                fail("ts_compile: asset path '{}' has conflicting published File identities.".format(published.path))
            available_assets[published.path] = (original, published)
        for original, coordinate in dependency_assets:
            relative = _layout_path(layout.root, coordinate)
            destination = out_base + "/" + relative
            previous = occupied.get(destination)
            if previous != None:
                canonical = canonical_assets.get((original, coordinate))
                if (canonical != None and previous != canonical) or (canonical == None and placed_assets.get(destination) != original):
                    fail("ts_compile: asset '{}' needs '{}' already occupied by '{}'; use distinct package-local data paths.".format(original.path, destination, previous.path))
                continue
            available = available_assets.get(destination)
            if available != None and available[0] != original:
                fail("ts_compile: asset '{}' needs '{}' already occupied by '{}'; use distinct package-local data paths.".format(original.path, destination, available[0].path))
            if is_javascript(original) and (original, coordinate) not in canonical_assets:
                fail("ts_compile: executable asset '{}' at '{}' has no live canonical File. Did you mean to retain its owning producer in deps?".format(original.short_path, coordinate))
            canonical = canonical_assets.get((original, coordinate), original)
            published = available[1] if available else canonical
            if available == None and destination != canonical.path:
                published = ctx.actions.declare_file(relative)
                if (original, coordinate) in canonical_assets:
                    ctx.actions.symlink(output = published, target_file = canonical)
                else:
                    runtime_copies.append((canonical, published))
            if (original, coordinate) in canonical_assets and published != canonical:
                canonical_links.append((published, canonical))
            occupied[destination] = published
            placed_assets[destination] = original
            data_staged.append(published)
            asset_files.append((original, coordinate, published))
    if manifest and (emit or layout.moved):
        prefix = manifest_source.short_path[:-len(manifest_source.basename)]
        logical_prefix = _logical_path(manifest_source)[:-len(manifest_source.basename)]
        targets = [
            {"source": "./" + coordinate[len(logical_prefix):], "runtime": "./" + published.path[len(out_base) + 1:]}
            for _original, coordinate, published in asset_files
            if coordinate.startswith(logical_prefix) and published.path.startswith(out_base + "/")
        ] + [
            {"source": "./" + source.short_path[len(prefix):], "runtime": "./" + runtime.path[len(out_base) + 1:]}
            for source, runtime in dict([(source, runtime) for source, runtime in data_sources.items() if source not in own_assets] + runtime_files.items()).items()
            if source.short_path.startswith(prefix) and runtime.path.startswith(out_base + "/")
        ]
        declaration_targets = [
            {"source": "./" + source.short_path[len(prefix):], "runtime": "./" + _package_relative_path(declaration, pkg)}
            for source, declaration in declaration_files.items()
            if source.short_path.startswith(prefix)
        ]
        targets += [
            {"source": "./" + _module_path(source, runtime)[len(logical_prefix):], "runtime": "./" + runtime.path[len(out_base) + 1:]}
            for source, runtime in runtime_files.items()
            if _logical_path(source).startswith(logical_prefix) and source != runtime and runtime.path.startswith(out_base + "/")
        ]
        declaration_targets += [
            {"source": "./" + _module_path(source, declaration)[len(logical_prefix):], "runtime": "./" + _package_relative_path(declaration, pkg)}
            for source, declaration in declaration_files.items()
            if _logical_path(source).startswith(logical_prefix) and source != declaration
        ]
        manifest_action(ctx, manifest_source, manifest, tsx_extension if emit else None, targets, declaration_targets if emit else None)
    module_pairs = runtime_files.items() + dependency_pairs + linked_modules
    module_pairs += [
        (source, runtime)
        for _owner, pairs in runtime_mappings(
            [struct(asset_files = tuple(asset_files), canonical_links = tuple(canonical_links))] + dependency_owners,
            {file: True for file in data_staged + dependency_data},
        )
        for source, runtime in pairs
        if is_javascript(runtime) and (source, runtime) not in module_pairs
    ]
    module_sources = {source: True for source, _runtime in module_pairs}
    runtime_scopes = {file: True for file in scope_inputs if file not in module_sources}
    incoming_data = {file: True for file in data_sources.keys() + data_staged + dependency_data}
    incoming_scopes = [
        pair
        for record in dependency_owners
        if hasattr(record, "runtime_scopes")
        for pair in record.runtime_scopes
        if pair[1] in incoming_data
    ]
    runtime_scopes.update({source: True for source, _runtime in incoming_scopes if source not in module_sources})
    runtime_scopes.update({
        file: True
        for record in dependency_owners
        for file in record.type_inputs.to_list()
        if file.basename == "package.json" and file in incoming_data and file not in module_sources and file != getattr(record, "replaced_scope", None)
    })
    data_staged, replaced_scopes, scope_pairs, scope_checks = _runtime_scope_data(
        ctx,
        runtime_scopes,
        [source for source in module_sources if source.basename == "package.json"],
        data_staged,
        dependency_data,
        data_sources,
        incoming_scopes,
        module_pairs + [(source, declaration) for source, declaration in declaration_files.items() if source not in runtime_files],
        preserve_sources = not emit and not layout.moved,
        canonical_links = canonical_links,
    )
    if any([scope in incoming_data for scope in replaced_scopes]):
        transitive_data_sets = [depset([
            file
            for file in dependency_data
            if file not in replaced_scopes
        ], order = "postorder")]

    # One action per copy: targets of one package that stage the same File share its action.
    for source, runtime in runtime_copies:
        ctx.actions.run(
            executable = get_tools_toolchain(ctx).tsaction,
            arguments = ["stage", "-out=" + out_base, source.path, runtime.path[len(out_base) + 1:]],
            inputs = [source],
            outputs = [runtime],
            mnemonic = "TsStage",
        )
    as_built = [manifest] if manifest else []
    all_outputs = (
        runtime_sources + js_outputs + js_map_outputs + js_passthrough + data_staged + as_built
    )

    program_srcs = compile_srcs + js_srcs
    check_srcs = compile_srcs + js_srcs + passthrough_dts
    joined = depset(transitive = joined_source_sets).to_list()

    direct_dts = depset(dts_outputs + published_dts + declaration_links, order = "postorder")
    package_scopes = [src for src in data_srcs if src.basename == "package.json"]

    # Consumers read the as-built manifest in place of the scope it replaces.
    replaced_scope = manifest_source if manifest and (emit or layout.moved) else None
    owners = depset(
        [struct(
            label = label_text(ctx.label),
            files = depset(
                check_srcs + dts_outputs + published_dts + declaration_links + data_staged + as_built + type_inputs + package_scopes,
            ),
            declarations = direct_dts,
            # Borrowed declarations stay at source paths, so consumers without the as-built manifest need the original scope.
            type_inputs = depset(type_inputs + package_scopes, transitive = [direct_dts if emit else depset(check_srcs)], order = "postorder"),
            runtime_files = tuple(runtime_files.items()),
            declaration_files = tuple(declaration_files.items()),
            canonical_links = tuple(canonical_links),
            asset_files = tuple(asset_files),
            runtime_scopes = scope_pairs,
            scope_manifest = manifest if replaced_scope else None,
            replaced_scope = replaced_scope,
            importers = tuple(own_importers),
            npm_bindings = tuple({(selected.name, selected.link.link, selected.link.store.tree): True for selected in lookup_npm}),
        )],
        transitive = owner_sets,
    )

    # tsc reads a JSON src on its own: an import resolves to it, and the nearest
    # package.json decides a module's format and the package's own name.
    json_srcs = [f for f in data_srcs if f.extension == "json"]

    # A dep's .json is typed from the file too: the closure's join the program
    # beside the declarations, and an import of one resolves in the sandbox.
    dep_json = [
        f
        for f in dependency_data
        if f.extension == "json"
    ]
    json_modules = {runtime: True for source, runtime in dependency_pairs if source.extension == "json"}

    # Package imports do not use rootDirs to reach generated compiler inputs.
    overlays.update({
        file.path: _logical_path(file)
        for file in compiler_type_files.keys() + [file for file in dep_json if file in json_modules]
        if not file.is_source and not file.is_directory
    })
    exact_overlays = [json.encode([declaration.path, _module_path(source, declaration)]) for source, declaration in dependency_declarations]
    exact_overlays += [json.encode([runtime.path, _logical_path(source)]) for source, runtime in dependency_pairs if source.extension == "json"]
    compiler_overlays = [json.encode([path, overlays[path]]) for path in sorted(overlays.keys())] + exact_overlays
    lint = ctx.attr._lint[LintConfigInfo]
    needs_config = program_srcs or (lint.binary and check_srcs and any(["{tsconfig}" in arg for arg in lint.args]))
    tsgo_toolchain_info = ctx.toolchains[TSGO_TOOLCHAIN_TYPE]
    if needs_config and not tsgo_toolchain_info:
        fail(("ts_compile: {} needs a tsgo toolchain, and none is registered." +
              "\nThe tsconfig the actions read, and the target and jsx oxc " +
              "transforms with, come from `tsgo --showConfig`.\nAdd to " +
              "MODULE.bazel:\n    register_toolchains(" +
              "\"@rules_typescript//ts/toolchain:all\")").format(ctx.label))

    tsconfig = None
    options_file = None
    if needs_config:
        tsgo = tsgo_toolchain_info.tsgo_info

        written = tsconfig_action(
            ctx,
            tsgo = tsgo,
            check_srcs = check_srcs + joined,
            tsconfig_chain = tsconfig_chain,
            baseline_file = baseline_file,
            dep_dts = dep_dts_depset,
            declared_jsx = declared_jsx,
            declared_module = declared_module,
            types_deps = sorted([
                info.package_name[len("@types/"):]
                for info in direct_npm_infos
                if info.package_name.startswith("@types/")
            ]),
            isolated_declarations = oxc_emits_dts,
            lib_check = ctx.attr._lib_check[BuildSettingInfo].value,
            emit = emit,
            declaration_paths = [_module_path(source, declaration) for source, declaration in dependency_declarations],
        )
        tsconfig = written.tsconfig
        options_file = written.options

    validation_outputs = []
    format_stamp = format_action(ctx, ctx.attr._format[FormatConfigInfo], ctx.files.srcs)
    if format_stamp:
        validation_outputs.append(format_stamp)
    program_inputs = check_srcs + joined + json_srcs + dep_json + dep_manifests
    generated_srcs = [
        file
        for file in check_srcs + joined + json_srcs + [file for file in compiler_type_files if is_javascript(file)]
        if not file.is_source
    ]
    if program_srcs:
        if emit and compile_srcs:
            emit_action(
                ctx,
                oxc = oxc,
                tsgo = tsgo,
                srcs = compile_srcs,
                roots = sorted(emit_roots.keys()),
                outputs = emit_outputs,
                out_base = out_base,
                tsconfig = tsconfig,
                chain = tsconfig_chain,
                importers = importers,
                inherited_importers = inherited_importers,
                overlays = compiler_overlays,
                manifests = dep_manifests,
                program_inputs = program_inputs,
                generated_srcs = generated_srcs,
                dep_dts = dep_dts_depset,
                npm_files = npm_files,
                options_file = options_file,
                scratch = "{}/{}.emit".format(tsconfig.dirname, ctx.label.name),
                source_map = source_map,
                emit_dts = oxc_emits_dts,
                declared_module = declared_module,
                es_modules = es_modules,
                runtime_scopes = scope_checks,
            )
        if twin_pairs:
            emit_action(
                ctx,
                oxc = oxc,
                tsgo = tsgo,
                srcs = compile_srcs,
                roots = sorted(emit_roots.keys()),
                outputs = [twin for _, twin in twin_pairs],
                out_base = "{}/{}".format(out_base, twins_dir),
                tsconfig = tsconfig,
                chain = tsconfig_chain,
                importers = importers,
                inherited_importers = inherited_importers,
                overlays = compiler_overlays,
                manifests = dep_manifests,
                program_inputs = program_inputs,
                generated_srcs = generated_srcs,
                dep_dts = dep_dts_depset,
                npm_files = npm_files,
                options_file = options_file,
                scratch = "{}/{}.emit".format(tsconfig.dirname, twins_dir),
                source_map = False,
                emit_dts = False,
                declared_module = declared_module,
                es_modules = True,
            )
        ownership = ownership_manifest(
            ctx,
            own = check_srcs + json_srcs + type_inputs,
            direct = direct_labels,
            owners = owners,
            npm_declared = [
                struct(name = selected.name, tree = selected.link.store.tree)
                for selected in selected_npm
            ],
            npm_reachable = npm_reachable,
        )
        validation_outputs.append(tsgo_check(
            ctx,
            tsgo = tsgo,
            tsconfig = tsconfig,
            importers = importers,
            inherited_importers = inherited_importers,
            overlays = compiler_overlays,
            manifests = dep_manifests,
            srcs = program_inputs,
            generated_srcs = generated_srcs,
            chain = tsconfig_chain,
            dep_dts = dep_dts_depset,
            npm_files = npm_files,
            ownership = ownership,
            checkers = checkers,
            runtime_scopes = [] if emit and compile_srcs else scope_checks,
        ))
        if tsgo_emits_dts:
            emit_action(
                ctx,
                oxc = oxc,
                tsgo = tsgo,
                srcs = declaration_srcs,
                roots = sorted(dts_roots.keys()),
                outputs = dts_outputs + dts_map_outputs,
                out_base = out_base,
                tsconfig = tsconfig,
                chain = tsconfig_chain,
                importers = importers,
                inherited_importers = inherited_importers,
                overlays = compiler_overlays,
                manifests = dep_manifests,
                program_inputs = program_inputs,
                generated_srcs = generated_srcs,
                dep_dts = dep_dts_depset,
                npm_files = npm_files,
                options_file = options_file,
                scratch = "{}/{}.declare".format(tsconfig.dirname, ctx.label.name),
                source_map = False,
                emit_dts = False,
                declared_module = declared_module,
                declarations_only = True,
                declaration_map = declaration_map,
                checkers = checkers,
            )

    elif scope_checks:
        stamp = ctx.actions.declare_file(ctx.label.name + ".tsscopecheck")
        args = ctx.actions.args()
        args.use_param_file("@%s", use_always = False)
        args.set_param_file_format("multiline")
        args.add(stamp, format = "-stamp=%s")
        scope_inputs = runtime_scope_inputs(args, scope_checks)
        ctx.actions.run(
            executable = get_tools_toolchain(ctx).tsaction,
            arguments = ["stamp", args],
            inputs = scope_inputs,
            outputs = [stamp],
            mnemonic = "TsScopeCheck",
            progress_message = "TsScopeCheck %{label}",
        )
        validation_outputs.append(stamp)

    if lint.binary and check_srcs:
        validation_outputs.append(lint_action(
            ctx,
            lint = lint,
            srcs = check_srcs,
            tsconfig = tsconfig,
            inputs = program_inputs,
            generated_srcs = generated_srcs,
            chain = tsconfig_chain,
            dep_dts = dep_dts_depset,
            npm_files = npm_files,
            importers = importers,
            inherited_importers = inherited_importers,
            overlays = compiler_overlays,
            manifests = dep_manifests,
        ))

    direct_runtime_sources = depset(runtime_sources, order = "postorder")
    transitive_runtime_sources = depset(
        runtime_sources,
        transitive = runtime_source_sets,
        order = "postorder",
    )
    direct_js = depset(js_outputs + js_passthrough, order = "postorder")
    direct_js_map = depset(js_map_outputs, order = "postorder")

    transitive_js = depset(
        js_outputs + js_passthrough,
        transitive = transitive_js_sets,
        order = "postorder",
    )
    transitive_js_map = depset(
        js_map_outputs,
        transitive = transitive_js_map_sets,
        order = "postorder",
    )
    transitive_data = depset(
        data_staged,
        transitive = transitive_data_sets,
        order = "postorder",
    )
    transitive_es_twins = depset(
        twin_pairs,
        transitive = transitive_es_twins_sets,
    )

    info = TsInfo(
        js = direct_js,
        runtime_sources = direct_runtime_sources,
        runtime_source_owners = depset(
            [label_text(ctx.label)] if runtime_sources else [],
            transitive = [dep[TsInfo].runtime_source_owners for dep in ctx.attr.deps],
        ),
        transitive_runtime_sources = transitive_runtime_sources,
        js_maps = direct_js_map,
        declarations = direct_dts,
        data = depset(data_staged, order = "postorder"),
        manifest = manifest,
        sources = depset(check_srcs, order = "postorder"),
        tsconfig = ctx.file.tsconfig,
        transitive_js = transitive_js,
        transitive_js_maps = transitive_js_map,
        transitive_data = transitive_data,
        transitive_es_twins = transitive_es_twins,
        npm_packages = depset(
            direct_npm_infos,
            transitive = dep_npm_package_sets,
            order = "postorder",
        ),
        npm_files = npm_files,
        owners = owners,
    )

    output_groups = {}
    if declarations:
        output_groups["declarations"] = depset(
            dts_map_outputs,
            transitive = [direct_dts],
        )

    # The tsconfig the compiler read, for a test comparing the build's
    # resolution with the editor's; a target with no program generates none.
    if tsconfig:
        output_groups["tsconfig"] = depset([tsconfig])
    if validation_outputs:
        output_groups["_validation"] = depset(validation_outputs)

    return struct(
        outputs = all_outputs + published_dts,
        js = js_outputs + js_passthrough,
        runtime_sources = runtime_sources,
        runtime_inputs = {
            source: runtime_files.get(source, input_canonical[source])
            for source in compile_srcs + js_srcs + json_srcs
        },
        layout = layout,
        transitive_runtime_sources = transitive_runtime_sources,
        importers = chain,
        npm_files = npm_files,
        packages = packages,
        transitive_js = transitive_js,
        transitive_data = transitive_data,
        es_twins = transitive_es_twins,
        info = info,
        output_groups = output_groups,
    )

def instrumented_files(ctx):
    # The runner reports on compiled .js; a .ts baseline would name the same code twice.
    return coverage_common.instrumented_files_info(
        ctx,
        source_attributes = ["srcs"],
        dependency_attributes = ["deps"],
        extensions = _INSTRUMENTED_EXTENSIONS,
        baseline_coverage_files = [],
    )

def _ts_compile_impl(ctx):
    program = compile_program(ctx, es_twins = True, package_data = ctx.files.data)

    # This target's own outputs; a dep's reach a consumer through TsInfo.
    return [
        DefaultInfo(files = depset(program.outputs)),
        program.info,
        instrumented_files(ctx),
        OutputGroupInfo(**program.output_groups),
    ]

TS_COMPILE_ATTRS = {
    "package_scopes": attr.label_list(
        doc = "Package.json metadata kept at its original compiler File identity and placed relative to published runtime modules. Unchanged source-mode placement retains the original scope File. A local package-root scope also supplies the npm publication manifest. Not source roots or JSON module endpoints. Files also in srcs retain their module role and layout checks.",
        allow_files = [".json"],
    ),
    "type_inputs": attr.label_list(
        doc = "Additional compiler inputs, such as declaration package scopes, staged at their original paths. Not source roots, runtime files or outputs. Files also in srcs retain their source role.",
        allow_files = True,
    ),
    "data": attr.label_list(
        doc = "Standalone asset files staged at package-relative paths in both emit modes. Put JSON module inputs in srcs and package metadata in package_scopes.",
        allow_files = True,
    ),
    "emit": attr.bool(
        default = False,
        doc = "Opt in to JavaScript and declaration emission for consumers that require built files. By default, publish TypeScript sources and retain validation.",
    ),
    "srcs": attr.label_list(
        doc = """The package's files.

.ts / .tsx      compiled; one .js (+ .js.map, + .d.ts) output each, from oxc
                for an ES-module program and from tsgo for a CommonJS-shaped
                one, as the tsconfig's `module` says. A program tsgo emits,
                declared by its ts_config's `module`, also gets the ES twin
                of each .js under <name>.es/, which a vitest test runs in
                place of the .js. A .tsx under jsx: preserve emits .jsx
                (+ .jsx.map), the name tsc gives it, when the tsconfig's
                ts_config declares that value.
.js / .mjs/.cjs retained in unchanged source-mode layouts, otherwise copied
                into the output tree; added to the type program. allowJs is
                set for them, so JSDoc types cross the package boundary;
                set checkJs in the tsconfig to have them
                type-checked. Under --//ts:declarations=tsgo each one also gets
                a declaration (.d.ts / .d.mts / .d.cts), the same as tsc,
                unless srcs already holds that file.
.d.ts / .d.mts / .d.cts
                declarations: type context for the check, passed straight
                through to consumers. One with no top-level import or export
                declares globals, and those are in scope in this target's own
                program; a consumer that wants them names the file in its own
                tsconfig `types`. A .d.mts is the declaration of the .mjs of
                the same stem, whether or not that .mjs is in srcs: "./x.mjs"
                resolves to x.d.mts, and a .mjs listed beside its .d.mts is
                staged but leaves the type program, as under tsc, so the
                checked-in file is its only declaration.
.json           retained in unchanged source-mode layouts, otherwise staged;
                a tsgo input typed from its contents under resolveJsonModule, which
                bundler resolution implies, and the nearest package.json decides
                a module's format and the package's own name. The package.json
                at the package's root is also written as built, every
                source-file target rewritten to the emitted file, as
                <name>.package.json: the manifest a dependent's program root
                lays at the package's path and a member's store tree copies. A
                test's runfiles hold the src as written.
anything else   staged into the output tree unchanged at its package-relative
                path, so the compiled module beside it reaches it by the same
                relative path at run time; never a tsgo input. A consumer gets
                the closure as TsInfo.transitive_data. A .mts or .cts is
                refused: the rule emits .js and .d.ts from .ts alone.

Paths are kept relative to the target's package, so srcs may span a subtree.
In source mode, foreign JSON module inputs keep their original paths. Put a
foreign standalone JSON asset in data to stage it at this package's path.
""",
        allow_files = True,
    ),
    "deps": attr.label_list(
        doc = """What this target imports: ts_compile, ts_codegen or
ts_npm_package targets, each providing TsInfo.

An npm dep reaches tsgo through the link of the nearest importer on the
`node_modules` chain that declares it, under its package name; a first-party
dep through its declarations, staged under bazel-bin at the paths the
tsconfig's `paths` and their bin-dir twins reach, or through a relative
import -- on a ts_test, a dep under the test's tsconfig through its sources
instead; a workspace member through its importer's link target,
`//<importer>:node_modules/<name>`; the package's own name, from a test or a
package below it, through the dep's manifest as built, which the program root
lays at the package's path, the dep being the package's ts_compile.""",
        providers = [TsInfo],
    ),
    "node_modules": attr.label(
        doc = """The `node_modules` target of the nearest lockfile importer at
or above this package: the chain a direct npm dep resolves along, nearest
importer first, as pnpm's walk-up from the importing file. The link whose
store is the dep's resolution is the one the program reads. source_node_modules
adds importer contexts for retained inputs. Each declared package must match
a nearest link's store File in at least one context; different importer locations
may select different stores. Required when the closure holds an npm package, a first-party
dep's included: its declarations import the packages it declared, and the
walk up from them ends at the chain's root. Gazelle writes it on every
target.""",
        providers = [NodeModulesInfo],
    ),
    "source_node_modules": attr.label_list(
        doc = "Source lookup contexts for compiler inputs whose npm lookups differ from the target's node_modules chain. Nearest links and their originating-chain @types companions preserve lookup precedence; each direct package must match a store File in a declared context. Gazelle derives these targets from retained inputs with compiler-observed npm imports.",
        providers = [NodeModulesInfo],
    ),
    "tsconfig": attr.label(
        doc = """The project's own tsconfig.json: where every compiler option
comes from.

Either a .json file or a ts_config target, which additionally declares the
files the tsconfig `extends`, with `jsx = "preserve"` that a .tsx emits .jsx,
and with `module` the kind tsgo emits, so the ES twins exist; the rule names
its outputs before any action reads the file, and the TsConfig action fails a
target when a declaration and the file disagree. The file is referenced where
it lives, not copied, so relative paths inside it keep resolving against the
directory they were written for.

The action's tsconfig extends the ruleset's baseline (strict, module Preserve,
target es2022, jsx react-jsx, skipLibCheck, esModuleInterop) and then this
file, so every key the file or its own extends chain mentions wins and only the
keys it says nothing about fall back to the baseline. Over both, tsaction sets
the keys Bazel owns -- rootDirs, the emit shape, `include` and `files` --
rewrites `paths` to the source and bin-dir twins of each value, and lists each
path-shaped `types` entry as a root file at its staged path; a `types` entry
naming a package resolves through the importer chain. oxc transforms with the
target, jsx and jsxImportSource tsgo reads from the same chain, and the
chain's `module` decides whether oxc or tsgo emits the JavaScript.

Without a tsconfig the baseline alone is the program's options.

moduleResolution the baseline never asserts: TypeScript couples it to `module`
and tsgo derives the resolver from whichever `module` wins, which is Bundler
for all of them but Node16/NodeNext.""",
        allow_single_file = [".json"],
    ),
    "_format": FORMAT_ATTR,
    "_lint": attr.label(
        default = Label("//ts:lint"),
        providers = [LintConfigInfo],
    ),
    "_declarations": attr.label(default = Label("//ts:declarations")),
    "_source_map": attr.label(default = Label("//ts:source_map")),
    "_declaration_map": attr.label(default = Label("//ts:declaration_map")),
    "_lib_check": attr.label(default = Label("//ts:lib_check")),
    "_checkers": attr.label(default = Label("//ts:checkers")),
}

TS_COMPILE_TOOLCHAINS = [
    OXC_TOOLCHAIN_TYPE,
    TOOLS_TOOLCHAIN_TYPE,
    config_common.toolchain_type(TSGO_TOOLCHAIN_TYPE, mandatory = False),
    config_common.toolchain_type(JS_TOOL_TOOLCHAIN_TYPE, mandatory = False),
]

ts_compile = rule(
    implementation = _ts_compile_impl,
    attrs = TS_COMPILE_ATTRS,
    toolchains = TS_COMPILE_TOOLCHAINS,
    doc = """Publishes TypeScript sources with tsgo validation by default.
Set emit=True to produce JavaScript and declarations.

With emit=True, produces one .js (+ .js.map under --//ts:source_map) and one .d.ts
per .ts/.tsx input -- .jsx and .jsx.map for a .tsx under jsx: preserve, as tsc
names them -- and stages every other src -- JavaScript, JSON, anything -- into
the output tree as-is. Output paths stay relative to the target's package, so
srcs may span a subtree.

The .d.ts are the compilation boundary: a dependent's program reads them and
nothing else of the target, a ts_test under the target's tsconfig apart, which
reads the sources. They are TsInfo.declarations and the
`declarations` output group, not a default output: a leaf's `bazel build`
runs the check alone, and the declarations are emitted when a dependent reads
them or `--output_groups=declarations` asks.

tsgo's check is a validation action in the _validation output group on every
target: it runs during `bazel build`, fails it on a type error and blocks no
dependent. --//ts:declarations decides who emits the .d.ts. Under "tsgo" (the
default) a second tsgo run emits them from the full program; under "oxc" oxc
emits them syntactically, which requires an explicit type on every export.
--//ts:declaration_map adds a .d.ts.map beside each declaration under the
tsgo emit; --//ts:lib_check checks the program's .d.ts closure too.

Compiler options come from the ruleset's baseline and from `tsconfig`, read
through `tsgo --showConfig`. npm packages reach tsgo through the importer
chain `node_modules` names, one link per name an importer declares.
""",
)

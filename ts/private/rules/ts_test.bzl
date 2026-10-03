"""ts_test: ts_compile's actions over the test files, run under a runner target.

The rule takes TS_COMPILE_ATTRS and the test attributes; compile_program
registers the actions a ts_compile over the same srcs would, less the
declaration emit, every dep under the test's tsconfig checked from its
sources, and the importer chain tsgo resolved against is what the tests run
in: the runfiles hold the chain's links and the store files the program
reaches at their own paths.
`runner` names the target providing TsTestRunnerInfo whose `launch` writes the
launcher config. docs/rules/ts-test.md is the reference.
"""

load(
    "//tools/launcher:launcher.bzl",
    "LAUNCHER_TOOLCHAINS",
    "NATIVE_EXECUTABLE_ATTRS",
    "NATIVE_PROGRAM_ATTRS",
    "NativeExecutableInfo",
    "complete_native_executable",
    "declare_launcher",
    "declare_runnable",
    "rlocation_path",
    "runfiles_link_path",
    "runfiles_root_path",
    "runfiles_scope_paths",
    "validate_runfiles_modules",
)
load("//ts/private:node_modules.bzl", "runfiles_dir", "runtime_npm_contexts")
load("//ts/private:providers.bzl", "NodeModulesInfo", "NpmLinkInfo", "TsInfo", "TsTestRunnerInfo", "canonical_runtime_file", "is_javascript", "require_emitted_inputs", "require_runtime_scopes", "runtime_links", "runtime_mappings", "runtime_scope_destinations")
load(
    "//ts/private:runtime.bzl",
    "JS_RUNTIME_TOOLCHAIN_TYPE",
    "get_js_runtime",
)
load("//ts/private:toolchain.bzl", "TOOLS_TOOLCHAIN_TYPE")
load("//ts/private/actions:workers_pool.bzl", "WORKERS_POOL_ATTRS")
load(
    "//ts/private/rules:ts_compile.bzl",
    "TS_COMPILE_ATTRS",
    "TS_COMPILE_TOOLCHAINS",
    "compile_program",
    "instrumented_files",
)

_ENTRY_EXTENSIONS = ["js", "jsx", "mjs", "cjs"]
_SOURCE_EXTENSIONS = ["ts", "tsx", "mts", "cts"]

def _same_package(a, b):
    return a.package == b.package and a.repo_name == b.repo_name

def _package_sources(ctx, program, runtime_path):
    dependencies = [dep[TsInfo] for dep in ctx.attr.deps if _same_package(dep.label, ctx.label)]
    source_sets = [depset(ctx.files.srcs)] + [info.sources for info in dependencies]
    pairs = program.runtime_inputs.items()
    if dependencies:
        pairs += [
            pair
            for owner in depset(transitive = [info.owners for info in dependencies]).to_list()
            for pair in getattr(owner, "runtime_files", ())
        ]
    replacements = [
        (source, runtime)
        for source, runtime in pairs
        if source != runtime and (rlocation_path(ctx, source) == runtime_path(runtime) or source.extension == runtime.extension)
    ]
    if not replacements:
        return source_sets

    # Owner records can outlive their runtime Files; only published modules replace source entries.
    live = {file: True for file in depset(transitive = [program.transitive_js, program.transitive_runtime_sources, program.transitive_data]).to_list()}
    replaced = {source: True for source, runtime in replacements if runtime in live}
    if not replaced:
        return source_sets
    return [depset([
        source
        for source in depset(transitive = source_sets).to_list()
        if source not in replaced
    ])]

def _chain(ctx, program):
    return struct(
        dirs = [importer.dir for importer in program.importers],
        rlocations = [
            runfiles_dir(ctx, importer.label)
            for importer in program.importers
        ],
        npm_files = program.npm_files,
    )

def _runtime_demands(ctx, program_inputs, test, owners, links, runtime_data, program_outputs, launched, package_sources, execution_paths, runtime_path, stage, canonical_links):
    # Runtime source sets also carry npm stores; only first-party owners identify their module members.
    javascript = {canonical_runtime_file(file, links): True for file in test.transitive_js.to_list()}
    modules = {canonical_runtime_file(file, links): True for file in program_inputs.values()} | javascript

    # A source-tree data module resolves its relative imports beside published outputs only when materialized.
    modules.update({
        canonical_runtime_file(file, links): True
        for file in ctx.files.data
        if is_javascript(file) and file.is_source
    })
    sources = {file: True for file in test.runtime_sources.to_list()}
    scope_files = javascript | sources
    module_sources = {}
    scoped_modules = {}
    live = modules | sources | runtime_data
    for _owner, pairs in runtime_mappings(owners, live):
        for source, runtime in pairs:
            modules[runtime] = True
            if is_javascript(runtime):
                scope_files[runtime] = True
            if runtime in scope_files:
                module_sources.setdefault(runtime, {})[source] = True
    modules.update({
        file: True
        for file in depset(transitive = [owner.files for owner in owners if not hasattr(owner, "runtime_files")]).to_list()
        if file in sources
    })

    # Runner links replace omitted Files (ES twins, for example); ordinary Files still take precedence.
    ordinary = {file: True for file in depset(
        program_outputs + launched.files,
        transitive = [launched.transitive_files] + package_sources,
    ).to_list()}
    ordinary_paths = {rlocation_path(ctx, file): True for file in ordinary}
    demanded = []
    published = dict(modules)
    selected_modules = {}
    replacements = getattr(launched, "replacements", {})
    for file in modules:
        link = runfiles_link_path(file)
        expected = replacements.get(file, file if file in ordinary else launched.symlinks.get(link, file))
        demanded.append((rlocation_path(ctx, file), expected))
        selected_modules[file] = expected
        published[expected] = True
        if file in scope_files and not expected.is_directory:
            scope_files[expected] = True
            if expected != file:
                module_sources.setdefault(expected, {}).update(module_sources.get(file, {}))
            scoped_modules[(runtime_path(file), expected)] = tuple(module_sources.get(file, {}))
    demanded.extend([
        (rlocation_path(ctx, file), file)
        for file in ordinary
        if file in published and file not in modules
    ])
    runtime_modules = {rlocation_path(ctx, file): True for file, selected in selected_modules.items() if not selected.is_directory}
    canonical_files = {file: True for file in selected_modules.values()} | {file: True for _path, file in execution_paths}
    for original, replacement in replacements.items():
        link = runfiles_link_path(original)
        if original in ordinary or launched.symlinks.get(link) != replacement:
            fail("ts_test {}: explicit replacement of '{}' must omit the original File and bind its path to '{}'.".format(ctx.label, original.path, replacement.path))
        demanded.append((rlocation_path(ctx, original), replacement))

    # A custom runner can retain the original module alongside an unused ES-twin link.
    twins = test.es_twins.to_list()
    module_files = published | {twin: True for _original, twin in twins}
    scope_files.update({twin: True for original, twin in twins if original in scope_files})
    for original, twin in twins:
        module_sources.setdefault(twin, {}).update(module_sources.get(original, {}))
    alias_paths = {}
    for link, canonical in canonical_links:
        path = runfiles_link_path(link)
        replacement = launched.symlinks.get(path)
        expected = selected_modules.get(canonical, canonical)
        if replacement != None and replacement != link and replacement != expected:
            fail("ts_test {}: dependency link '{}' must reach selected canonical File '{}', not '{}'; remove the conflicting runner mapping.".format(ctx.label, link.short_path, expected.path, replacement.path))
        alias_paths[rlocation_path(ctx, link)] = True
        demanded.append((rlocation_path(ctx, link), replacement or link))
    for path, file in launched.symlinks.items():
        path = runfiles_root_path(ctx, path)
        if path in alias_paths:
            continue
        if file not in module_files or path not in ordinary_paths:
            demanded.append((path, file))
            if file in module_files and file not in canonical_files and not file.is_directory:
                runtime_modules[path] = True
                if file in scope_files:
                    scoped_modules[(path, file)] = tuple(module_sources.get(file, {}))
    demanded.extend(execution_paths)
    runtime_modules.update({path: True for path, file in execution_paths if not file.is_directory})
    scoped_modules.update({
        (path, file): tuple(module_sources.get(file, {}))
        for path, file in execution_paths
        if file in scope_files and not file.is_directory
    })
    demanded.extend([
        (rlocation_path(ctx, file), file)
        for file in ordinary
        if rlocation_path(ctx, file) in stage
    ])
    module_paths = {file: (rlocation_path(ctx, file), selected) for file, selected in selected_modules.items()}
    return demanded, runtime_modules, scoped_modules, ordinary, module_paths

def _layout_data(ctx, layout, files):
    # Moved modules read relative data from the shared layout, as ts_compile assets do.
    if not layout.moved:
        return {}
    package = "/".join([part for part in [ctx.label.workspace_root, ctx.label.package] if part])
    placed = {}
    for file in files:
        logical = runfiles_link_path(file)
        if layout.root and not logical.startswith(layout.root + "/"):
            continue
        destination = package + "/" + (logical[len(layout.root) + 1:] if layout.root else logical)
        if destination != logical:
            placed[destination] = file
    return placed

def _nearest_scope(files, path):
    parts = path.split("/")
    for depth in range(len(parts) - 1, -1, -1):
        scope = "/".join(parts[:depth] + ["package.json"])
        if scope in files:
            return (scope, files[scope])
    return None

def _scope_description(scope):
    if scope == None:
        return "no package scope"
    path, file = scope
    return "'{}' ({})".format(path, file.path if file != None else "empty entry")

def _ancestor_scope_links(ctx, runfiles, owners, links, scoped_modules):
    """Places each unchanged scope where its relocated modules find it first (ts-compile.md, Shared Source Layout)."""
    published = {}
    scopes = {}
    for owner in owners:
        for source, runtime in list(getattr(owner, "runtime_files", ())) + list(getattr(owner, "runtime_scopes", ())):
            if source.basename == "package.json":
                scopes[source] = True
                if canonical_runtime_file(runtime, links) == source:
                    published.setdefault(source, {})[runtime] = True
    if not published:
        return {}
    present = runfiles_scope_paths(ctx, runfiles)
    workspace = ctx.workspace_name + "/"
    namespace = workspace + (ctx.label.package + "/" if ctx.label.package else "")
    placed = {}
    for (path, runtime), sources in scoped_modules.items():
        module_dir = path.split("/")[:-1]
        for source in sources:
            for scope, destination in runtime_scope_destinations(scopes.keys(), source, path).items():
                runtimes = published.get(scope, {}).keys()
                if destination == None or len(runtimes) != 1 or not destination.startswith(workspace):
                    continue
                own = {scope: True} | published[scope]

                def foreign(file):
                    return file not in own and canonical_runtime_file(file, links) not in own

                # A layout move can put the module under another package's authored manifest, which then shadows its scope.
                location = destination
                moved = module_dir != rlocation_path(ctx, source).split("/")[:-1]
                for depth in range(len(destination.split("/")), len(module_dir) + 1):
                    candidate = "/".join(module_dir[:depth] + ["package.json"])
                    occupant = present.get(candidate)
                    if moved and occupant != None and occupant.is_source and rlocation_path(ctx, occupant) == candidate and foreign(occupant):
                        if depth == len(module_dir):
                            fail("ts_test {}: '{}' shadows package scope '{}' for '{}', and no directory between them can hold that scope. Did you mean to keep the module outside that package's directory?".format(ctx.label, candidate, scope.short_path, runtime.short_path))
                        location = "/".join(module_dir[:depth + 1] + ["package.json"])
                if location == destination and (destination in present or destination.startswith(namespace)):
                    continue
                occupant = present.get(location, placed.get(location, (None, None))[1])
                previous = placed.get(location, (scope, None))[0]
                if (location in present and foreign(occupant)) or previous != scope:
                    other = previous.short_path if previous != scope else (occupant.short_path if occupant != None else "an empty entry")
                    fail("ts_test {}: package scopes '{}' and '{}' both need runtime coordinate '{}'. Did you mean to keep those modules in separate package directories?".format(ctx.label, scope.short_path, other, location))
                if location not in present:
                    placed[location] = (scope, runtimes[0])
    return placed

def _ts_test_impl(ctx):
    runner = ctx.attr.runner[TsTestRunnerInfo]
    supports_sources = getattr(runner, "supports_source_inputs", False)
    program = compile_program(
        ctx,
        es_modules = runner.es_modules,
        package_program = True,
        declarations = False,
    )

    def runtime_path(file):
        return rlocation_path(ctx, file)

    package_sources = _package_sources(ctx, program, runtime_path)

    linked = {info.package_name: True for info in program.packages}
    missing = [name for name in runner.packages if name not in linked]
    if missing:
        fail(("ts_test {}: {} runs the tests and needs {} in the " +
              "npm closure, which no dep provides.\nAdd the hub label " +
              "of each to deps.").format(
            ctx.label,
            ctx.attr.runner.label,
            ", ".join(missing),
        ))

    entry_extensions = _ENTRY_EXTENSIONS + (_SOURCE_EXTENSIONS if program.runtime_sources else [])
    executable = {f: True for f in program.js + program.runtime_sources if f.extension in entry_extensions}
    runtime_inputs = {source: runtime for source, runtime in program.runtime_inputs.items() if runtime in executable}
    if ctx.attr.test_srcs:
        sources = {f: True for f in ctx.files.srcs}
        selected = {src: True for src in ctx.files.test_srcs}
        for src in ctx.files.test_srcs:
            if src not in sources:
                fail(("ts_test {}: test_srcs file '{}' is not in srcs. " +
                      "Did you mean to add it to the compiler inputs in srcs?").format(ctx.label, src.short_path))
        runtime_inputs = {source: runtime for source, runtime in runtime_inputs.items() if source in selected}
        if not runtime_inputs:
            fail(("ts_test {}: test_srcs selects no executable program inputs. " +
                  "Did you mean to select a TypeScript or JavaScript test from srcs?").format(ctx.label))
    entry_points = runtime_inputs.values()

    test_files_list = ctx.actions.declare_file(
        "{}_test_files.txt".format(ctx.label.name),
    )
    ctx.actions.write(
        output = test_files_list,
        content = "\n".join([
            runtime_path(f)
            for f in entry_points
        ]) + "\n",
    )

    runtime_binary = None
    runtime_args = []
    js_runtime = get_js_runtime(ctx)
    if js_runtime:
        runtime_binary = js_runtime.runtime_binary
        runtime_args = js_runtime.args_prefix

    # The workspace members in the closure: the packages with no extracted
    # manifest.
    members = [info for info in program.packages if info.package_dir == None]
    chain = _chain(ctx, program)

    data_targets = [target for target in ctx.attr.data if TsInfo in target]
    data_infos = [target[TsInfo] for target in data_targets]
    runtime_infos = [program.info] + data_infos
    owner_sets = depset(transitive = [info.owners for info in runtime_infos])
    runtime_js = depset(transitive = [info.transitive_js for info in runtime_infos])
    runtime_sources = depset(transitive = [info.transitive_runtime_sources for info in runtime_infos])

    # Runner placement needs the owners of every admitted compiler output, including data.
    owners = owner_sets.to_list()
    data_files = depset(transitive = [target[DefaultInfo].files for target in data_targets], order = "postorder")
    data_outputs = {file: True for file in data_files.to_list()}
    runtime_data_sets = [info.transitive_data for info in runtime_infos] + [data_files]
    runtime_data = {file: True for file in depset(transitive = runtime_data_sets, order = "postorder").to_list()}
    live_modules = runtime_data | {file: True for file in depset(transitive = [runtime_js, runtime_sources]).to_list()}
    runtime_files = tuple([
        (source, runtime)
        for _owner, pairs in runtime_mappings(owners, live_modules)
        for source, runtime in pairs
        if source not in program.runtime_inputs or runtime == program.runtime_inputs[source]
    ])
    asset_files = tuple([
        (original, coordinate, published)
        for owner in owners
        for original, coordinate, published in getattr(owner, "asset_files", ())
        if published in runtime_data
    ])
    links = runtime_links(owners)
    canonical_links = tuple([
        (link, canonical_runtime_file(link, links))
        for link in links
        if link in runtime_data
    ])
    test = struct(
        entry_points = entry_points,
        entry_extensions = entry_extensions,
        test_files_list = test_files_list,
        chain = chain,
        transitive_js = runtime_js,
        runtime_sources = runtime_sources,
        runtime_inputs = runtime_inputs,
        runtime_files = runtime_files,
        asset_files = asset_files,
        canonical_links = canonical_links,
        es_twins = depset(transitive = [info.transitive_es_twins for info in runtime_infos]),
        placed = {},
        runtime_data_sets = runtime_data_sets + [runtime_sources],
        package_sources = package_sources,
        inline_members = sorted({m.package_name: True for m in members}.keys()),
        runner = runner,
    )
    launched = runner.launch(ctx, test)

    replaced_links = {
        link: True
        for link, _canonical in canonical_links
        if launched.symlinks.get(runfiles_link_path(link), link) != link
    }
    replacements = getattr(launched, "replacements", {})
    program_outputs = [file for file in program.outputs if file not in replaced_links and file not in replacements]
    if replacements:
        package_sources = [depset([file for file in depset(transitive = package_sources).to_list() if file not in replacements])]
    uses_generated_test_list = (
        launched.mode == "node_test" and
        launched.section.get("test_files_list") == rlocation_path(ctx, test_files_list)
    )

    stage = launched.section.get("stage", {}) if launched.mode == "vitest" else {}
    demanded, runtime_modules, scoped_modules, ordinary, module_paths = _runtime_demands(
        ctx,
        program.runtime_inputs,
        test,
        owners,
        links,
        runtime_data,
        program_outputs,
        launched,
        package_sources,
        [(runtime_path(file), replacements.get(file, file)) for file in entry_points],
        runtime_path,
        stage,
        canonical_links,
    )

    section = dict(launched.section)
    context_inputs = []
    if launched.mode in ["node", "node_test", "vitest"]:
        available = {rlocation_path(ctx, file): file for file in ordinary} | dict(demanded)
        contexts, context_inputs = runtime_npm_contexts(ctx, struct(owners = owner_sets), module_paths, available, links)
        if contexts:
            section["npm_contexts"] = contexts
        demanded.extend([(rlocation_path(ctx, file), file) for file in context_inputs])

    config = {
        "label": str(ctx.label),
        "mode": launched.mode,
        "workspace": ctx.workspace_name,
        "runtime_args": runtime_args,
        "runtime_modules": sorted(runtime_modules),
        "env": launched.env,
        launched.mode: section,
    }
    if runtime_binary:
        config["runtime"] = rlocation_path(ctx, runtime_binary)

    config_importers = ctx.attr.config_node_modules + ([ctx.attr.workers_pool] if ctx.attr.workers_pool else [])
    files = (
        [test_files_list] + program_outputs +
        [file for file in ctx.files.data if file not in data_outputs or (file not in replaced_links and file not in replacements)] + ctx.files.config_node_modules + ctx.files.workers_pool + launched.files + context_inputs
    )
    runfiles = ctx.runfiles(
        files = files,
        transitive_files = depset(
            transitive = [launched.transitive_files, chain.npm_files] + [info.npm_files for info in data_infos] + package_sources,
        ),
        symlinks = _layout_data(ctx, program.layout, [file for file in ctx.files.data if file not in data_outputs]) | launched.symlinks,
    )
    for target in ctx.attr.data + config_importers:
        runfiles = runfiles.merge(target[DefaultInfo].default_runfiles)
    staged_scopes = _ancestor_scope_links(ctx, runfiles, owners, links, scoped_modules)
    workspace = ctx.workspace_name + "/"
    runfiles = runfiles.merge(ctx.runfiles(symlinks = {location[len(workspace):]: runtime for location, (_scope, runtime) in staged_scopes.items()}))
    staged_scopes = {location: scope for location, (scope, _runtime) in staged_scopes.items()}

    visible = runfiles_scope_paths(ctx, runfiles, [path for path, _file in demanded] + stage.keys() + stage.values())
    if stage:
        source_demands = [(path, file) for path, file in demanded if path in stage]
        validate_runfiles_modules(ctx, visible, source_demands, "ts_test")
        sources = dict(source_demands)
        staged = dict(visible)
        staged_demands = []
        for source in sorted(stage):
            file = sources.get(source)
            if file == None:
                fail(("ts_test {}: cannot establish config staging source '{}'. " +
                      "Did you mean to remove the conflicting data or runfiles entry?").format(ctx.label, source))
            staged[stage[source]] = file
            staged_demands.append((stage[source], file))
        for path, _file in scoped_modules:
            before = _nearest_scope(visible, path)
            after = _nearest_scope(staged, path)
            if before != after:
                fail(("ts_test {}: config staging changes the published runtime package scope for '{}' from {} to {}. " +
                      "Did you mean to place the config in a separate package scope, or use source mode " +
                      "with the same authored scope File?").format(ctx.label, path, _scope_description(before), _scope_description(after)))
        visible = staged
        demanded.extend(staged_demands)
    validate_runfiles_modules(ctx, visible, demanded, "ts_test")

    requirement = "runner {}".format(ctx.attr.runner.label)
    if not supports_sources:
        require_emitted_inputs(ctx.label, program.info, requirement)
    require_runtime_scopes(
        ctx.label,
        program.info,
        requirement,
        runtime_files = visible,
        module_paths = [
            (source, runtime, path)
            for (path, runtime), sources in scoped_modules.items()
            for source in sources
        ],
        owners = owners,
        staged_scopes = staged_scopes,
    )

    if not supports_sources and not uses_generated_test_list:
        def output_runtime_path(file):
            return rlocation_path(ctx, file)

        require_runtime_scopes(ctx.label, program.info, requirement, runtime_path = output_runtime_path, runtime_files = visible)
        if launched.mode not in ["node", "node_test"] or not config["runtime_modules"]:
            require_runtime_scopes(ctx.label, program.info, requirement, runtime_files = {file.path: file for file in visible.values() if file != None})

    runfiles = runfiles.merge(ctx.runfiles(root_symlinks = {
        path: file
        for path, file in visible.items()
        if file != None and "external/" + path in launched.symlinks
    }))
    launcher = declare_launcher(
        ctx,
        config,
        basename = "{}_test_launcher".format(ctx.label.name),
        runfiles = runfiles,
        runtime_file = runtime_binary,
        canonical_links = [(link, module_paths.get(canonical, (None, canonical))[1]) for link, canonical in canonical_links],
    )
    runfiles = runfiles.merge(ctx.runfiles(files = launcher.files + ([runtime_binary] if runtime_binary else []), root_symlinks = launcher.root_symlinks))

    providers = [
        DefaultInfo(executable = launcher.executable, runfiles = runfiles),
        NativeExecutableInfo(launcher = launcher, default_files = None),
    ]
    output_groups = program.output_groups | launched.output_groups
    if output_groups:
        providers.append(OutputGroupInfo(**output_groups))
    return providers

_TEST_ATTRS = {
    "test_srcs": attr.label_list(
        doc = "Execution roots selected from srcs before sharding. Empty uses all executable srcs. Other srcs remain compiler and runtime inputs.",
        allow_files = True,
    ),
    "runner": attr.label(
        doc = "The target that runs the compiled tests: //ts/runners:vitest " +
              "(the default), //ts/runners:node_test for tests written " +
              "against node:test, or any target providing " +
              "TsTestRunnerInfo. A vitest attribute set under another " +
              "runner is an analysis error rather than a silently dropped " +
              "setting.",
        default = Label("//ts/runners:vitest"),
        providers = [TsTestRunnerInfo],
    ),
    "env": attr.string_dict(
        doc = "Additional environment variables for the test.",
    ),
    "config": attr.label(
        doc = "The vitest config file (.ts/.mts/.cts/.js/.mjs/.cjs), merged " +
              "over the Bazel layer (root, cacheDir, the module ids, " +
              "server.fs.allow, coverage.allowExternal), so every " +
              "vitest setting is the file's, as under plain vitest. A " +
              "config that default-exports an array is read as a list of " +
              "vitest projects (test.projects). The modules it imports " +
              "relatively are `config_srcs`.",
        allow_single_file = [".ts", ".mts", ".cts", ".js", ".mjs", ".cjs"],
    ),
    "config_srcs": attr.label_list(
        doc = "The modules `config` imports relatively, and theirs, each " +
              "written at its own path in the runfiles tree, where the " +
              "config's imports resolve as in the checkout. Staging cannot replace a published runtime File " +
              "or change a JavaScript or TypeScript module's nearest package scope. Use a separate config scope or source mode when they overlap.",
        allow_files = True,
    ),
    "config_node_modules": attr.label_list(
        doc = "The importers or member links declaring the authored config's npm packages, " +
              "including those its relative modules import. Their links and " +
              "store files enter runfiles at their owner paths, without " +
              "changing the test program's node_modules or deps. Gazelle " +
              "derives this list separately from authored data.",
        providers = [[NodeModulesInfo], [NpmLinkInfo]],
    ),
    "data": attr.label_list(
        doc = "Extra runfiles for the test: fixtures, anything read at run " +
              "time. TsInfo targets retain their declared runtime closure and npm bindings without joining the compiler program or test entry roots.",
        allow_files = True,
    ),
    "coverage_provider": attr.string(
        doc = "Vitest coverage provider (test.coverage.provider): \"v8\" " +
              "(vitest's own default) or \"istanbul\".  The matching " +
              "@vitest/coverage-* package must be in the target's deps.  A " +
              "test whose pool runs in a second runtime needs \"istanbul\", " +
              "which instruments at transform time; v8 reads counters out " +
              "of node's inspector, which such a runtime does not have.",
        default = "",
        values = ["", "v8", "istanbul"],
    ),
}

_ts_test_program = rule(
    implementation = _ts_test_impl,
    executable = True,
    attrs = TS_COMPILE_ATTRS | _TEST_ATTRS | WORKERS_POOL_ATTRS | NATIVE_PROGRAM_ATTRS,
    fragments = ["platform"],
    toolchains = TS_COMPILE_TOOLCHAINS + LAUNCHER_TOOLCHAINS + [
        config_common.toolchain_type(JS_RUNTIME_TOOLCHAIN_TYPE, mandatory = False),
    ],
)

def _ts_test_executable_impl(ctx):
    return complete_native_executable(ctx, ctx.label.name + "_test_launcher") + [instrumented_files(ctx)]

ts_test = rule(
    implementation = _ts_test_executable_impl,
    test = True,
    attrs = dict(
        TS_COMPILE_ATTRS | _TEST_ATTRS | WORKERS_POOL_ATTRS | NATIVE_EXECUTABLE_ATTRS,
        # Bazel reads a test's coverage merger from this attribute; the target
        # runs the tools toolchain's, docs/rules/ts-test.md § Coverage.
        _lcov_merger = attr.label(
            cfg = "exec",
            default = Label("//ts/toolchain:lcov_merger_resolved"),
            executable = True,
        ),
    ),
    toolchains = [TOOLS_TOOLCHAIN_TYPE],
    doc = """Compiles TypeScript tests with ts_compile's actions and runs them.

srcs, deps, tsconfig and node_modules are ts_compile's; a dep under the test's
tsconfig is checked from its sources, one program with the package's compile,
and a dep under another through its declarations. The program emits no
declarations of its own; a file of another package the tests import is a src,
placed with the test's files in the shared source layout. The importer chain tsgo
checked the tests against is what they run in: the chain's links and the store
files the program reaches sit in the runfiles at their own paths. `runner`
names the target that runs the compiled files, //ts/runners:vitest by default;
`config`, `config_srcs`, `config_node_modules`, `workers_pool`, `coverage_provider` and
`wrangler_config` are the vitest runner's; `data` adds runfiles on either runner.
""",
)

def ts_test_runnable(name, **kwargs):
    declare_runnable(name, _ts_test_program, ts_test, kwargs, test = True)

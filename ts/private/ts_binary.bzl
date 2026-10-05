"""ts_binary: runs a compiled entry point, or a BundlerInfo's bundle of it.

entry_point is a ts_compile target (or any target providing TsInfo) or a plain
.js/.mjs/.cjs file. With a `bundler` the bundle is what runs; without one the
entry's .js runs directly. The launcher (//tools/launcher, driven by the
generated JSON config) resolves every path through the runfiles library, runs
the JS runtime toolchain's node when one is registered and the system `node`
otherwise, prepends the toolchain's args_prefix, and forwards every argument
after `--`. `TS_LAUNCHER_DUMP_CONFIG=1 bazel run //target` prints what it
would exec.
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
    "runfiles_scope_paths",
    "validate_runfiles_modules",
)
load("//ts/private:bundle_action.bzl", "create_bundle_action")
load("//ts/private:node_modules.bzl", "runfiles_dir", "runtime_npm_contexts")
load(
    "//ts/private:providers.bzl",
    "BundlerInfo",
    "NodeModulesInfo",
    "TsInfo",
    "canonical_runtime_file",
    "is_javascript",
    "require_emitted_inputs",
    "require_runtime_scopes",
    "runtime_links",
    "runtime_mappings",
    "ts_info",
)
load("//ts/private:runtime.bzl", "JS_RUNTIME_TOOLCHAIN_TYPE", "get_js_runtime")
load("//ts/private:toolchain.bzl", "TOOLS_TOOLCHAIN_TYPE")

_JS_ENTRY_EXTENSIONS = [".js", ".mjs", ".cjs"]

_TS_ENTRY_EXTENSIONS = [".ts", ".tsx", ".mts", ".cts"]

def _has_extension(file, extensions):
    for ext in extensions:
        if file.basename.endswith(ext):
            return True
    return False

def _js_file_entry(ctx, data_files, data_modules):
    """The TsInfo a plain JavaScript file at entry_point stands in for."""
    files = ctx.files.entry_point
    label = ctx.attr.entry_point.label
    entry = files[0] if len(files) == 1 else None
    if entry and _has_extension(entry, _JS_ENTRY_EXTENSIONS):
        return ts_info(
            js = depset([entry]),
            data = depset([file for file in data_files if file not in data_modules]),
            transitive_js = depset([entry] + data_modules),
        )
    if entry and _has_extension(entry, _TS_ENTRY_EXTENSIONS):
        fail(
            ("ts_binary: entry_point '{}' is a TypeScript source, which this " +
             "rule does not compile.\nCompile it with ts_compile and point " +
             "entry_point at that target, or hand entry_point an " +
             "already-plain .js/.mjs/.cjs file.").format(label),
        )
    fail(
        ("ts_binary: entry_point '{}' does not provide TsInfo and is not a " +
         "JavaScript file.\nThe entry_point attr must be a ts_compile target " +
         "(or any target that provides TsInfo), or a single {} file.\nDid " +
         "you mean: entry_point = \"//path/to:your_ts_compile_target\"?")
            .format(label, "/".join(_JS_ENTRY_EXTENSIONS)),
    )

def _entry_js_file(ctx, entry):
    """The one .js the launcher runs, out of the entry's direct outputs."""
    label = ctx.attr.entry_point.label
    entry_js_files = entry.js.to_list()
    names = ", ".join([f.short_path for f in entry_js_files])
    if not entry_js_files:
        fail(
            ("ts_binary: entry_point '{}' provides TsInfo but has no direct " +
             ".js outputs.\nEnsure the ts_compile target at entry_point has " +
             "at least one .ts source file in srcs.").format(label),
        )
    origins = {}
    for owner in entry.owners.to_list():
        for source, runtime in getattr(owner, "runtime_files", ()):
            if runtime in entry_js_files:
                origins.setdefault(runtime, {})[source] = True
    if ctx.attr.entry_file:
        wanted = ctx.attr.entry_file
        for ext in [".ts", ".tsx"]:
            if wanted.endswith(ext):
                wanted = wanted[:-len(ext)] + ".js"
        match = []
        for file in entry_js_files:
            sources = origins.get(file, {})
            if file.basename == wanted or any([ctx.attr.entry_file == source.basename for source in sources]):
                match.append(file)
        if len(match) != 1:
            fail(
                ("ts_binary: entry_file '{}' does not select exactly one output in entry_point '{}'." +
                 "\nAvailable .js files: {}. Did you mean to name a unique source filename, or use an entry_point with one entry output?").format(
                    ctx.attr.entry_file,
                    label,
                    names,
                ),
            )
        return match[0]
    if len(entry_js_files) == 1:
        return entry_js_files[0]
    candidates = entry_js_files if not origins else []
    if len(origins) == len(entry_js_files) and all([len(sources) == 1 for sources in origins.values()]):
        candidates = [
            file
            for file, sources in origins.items()
            for source in sources
            if source.owner.package == label.package and source.owner.repo_name == label.repo_name
        ]
        if len(candidates) == 1:
            return candidates[0]
    if origins:
        index_match = [file for file in candidates if any([source.basename in ["index.ts", "index.tsx", "index.js"] for source in origins[file]])]
    else:
        index_match = [file for file in candidates if file.basename == "index.js"]
    if len(index_match) == 1:
        return index_match[0]
    fail(
        ("ts_binary: entry_point '{}' produces {} .js files: {}.\nSet " +
         "entry_file to a unique source filename. Did you mean " +
         "entry_file = \"main.ts\"? Imported helpers do not select the entry.").format(label, len(entry_js_files), names),
    )

def _ts_binary_impl(ctx):
    entry_point = ctx.attr.entry_point
    data_files = ctx.files.data
    data_modules = [file for file in data_files if is_javascript(file) and not file.is_symlink]
    if TsInfo in entry_point:
        entry = entry_point[TsInfo]
    else:
        entry = _js_file_entry(ctx, data_files, data_modules)
    data_infos = [target[TsInfo] for target in ctx.attr.data if TsInfo in target]
    runtime_infos = [entry] + data_infos

    require_emitted_inputs(ctx.label, entry, "ts_binary")

    runtime_binary = None
    runtime_args = []
    js_runtime = get_js_runtime(ctx)
    if js_runtime:
        runtime_binary = js_runtime.runtime_binary
        runtime_args = js_runtime.args_prefix

    entry_file = _entry_js_file(ctx, entry)
    bundle_out = None
    extra_outputs = []
    if ctx.attr.bundler and BundlerInfo in ctx.attr.bundler:
        bundle_filename = ctx.attr.bundle_name if ctx.attr.bundle_name else ctx.attr.public_name
        bundle_result = create_bundle_action(ctx, entry, entry_file, bundle_filename)
        bundle_out = bundle_result.bundle_out
        extra_outputs = bundle_result.outputs
        entry_file = bundle_out
        info = ts_info(
            js = depset([bundle_out]),
            transitive_data = entry.transitive_data,
        )
    else:
        info = entry
    owner_sets = depset(transitive = [info.owners] + [runtime.owners for runtime in data_infos])

    runtime_depset = depset(
        transitive = [files for runtime in runtime_infos for files in [
            runtime.transitive_js,
            runtime.transitive_runtime_sources,
            runtime.transitive_js_maps,
            runtime.transitive_data,
            runtime.npm_files,
        ]],
    )

    node_modules = ctx.attr.node_modules
    node_modules_files = depset()
    if node_modules:
        node_modules_files = node_modules[DefaultInfo].files

    # Final runfiles admission needs each published module's File identity, including mapped JSON.
    owners = owner_sets.to_list()
    links = runtime_links(owners)
    module_files = depset(transitive = [info.transitive_js] + [runtime.transitive_js for runtime in data_infos])
    modules = {canonical_runtime_file(file, links): True for file in module_files.to_list() + data_modules}
    runtime_data = depset(transitive = [info.transitive_data] + [runtime.transitive_data for runtime in data_infos])
    live = modules | {file: True for file in runtime_data.to_list()}
    mappings = runtime_mappings(owners, live)
    scoped_modules = [
        (source, runtime, rlocation_path(ctx, runtime))
        for _owner, pairs in mappings
        for source, runtime in pairs
        if runtime in modules or is_javascript(runtime)
    ]
    modules.update({
        runtime: True
        for _owner, pairs in mappings
        for _source, runtime in pairs
    })
    demanded = [(rlocation_path(ctx, entry_file), entry_file)]
    demanded.extend([(rlocation_path(ctx, file), file) for file in modules])

    config = {
        "label": str(ctx.label),
        "mode": "node",
        "workspace": ctx.workspace_name,
        "runtime_args": runtime_args,
        "runtime_modules": sorted({path: True for path, file in demanded if not file.is_directory}),
        "node": {
            "entry": rlocation_path(ctx, entry_file),
        },
    }
    if runtime_binary:
        config["runtime"] = rlocation_path(ctx, runtime_binary)
    if node_modules:
        config["node"]["node_modules"] = runfiles_dir(ctx, node_modules.label)
    module_paths = {file: (rlocation_path(ctx, file), file) for file in modules}
    available = {rlocation_path(ctx, file): file for file in live}
    contexts, binding_sets, context_inputs = runtime_npm_contexts(ctx, struct(owners = owner_sets), module_paths, available, links)
    if contexts:
        config["node"]["npm_contexts"] = contexts
        config["node"]["npm_binding_sets"] = binding_sets
    demanded.extend([(rlocation_path(ctx, file), file) for file in context_inputs])

    explicit_runfiles = list(data_files) + context_inputs
    if bundle_out:
        explicit_runfiles.append(bundle_out)
        explicit_runfiles.extend(extra_outputs)

    runfiles = ctx.runfiles(
        files = explicit_runfiles,
        transitive_files = depset(
            transitive = [runtime_depset, node_modules_files],
        ),
    )

    visible = runfiles_scope_paths(ctx, runfiles, [path for path, _file in demanded])
    validate_runfiles_modules(ctx, visible, demanded, "ts_binary")

    require_runtime_scopes(
        ctx.label,
        info,
        "ts_binary",
        runtime_files = visible,
        module_paths = scoped_modules,
        owners = owners,
    )

    launcher = declare_launcher(
        ctx,
        config,
        runfiles = runfiles,
        runtime_file = runtime_binary,
        canonical_links = [pair for owner in owners for pair in getattr(owner, "canonical_links", ())],
    )
    runfiles = runfiles.merge(ctx.runfiles(files = launcher.files + ([runtime_binary] if runtime_binary else []), root_symlinks = launcher.root_symlinks))

    if bundle_out:
        output_group = OutputGroupInfo(
            bundle = depset([bundle_out]),
            js_tree = entry.transitive_js,
        )
        default_files = depset(extra_outputs)
    else:
        output_group = OutputGroupInfo(js_tree = entry.transitive_js)
        default_files = depset([])

    return [
        DefaultInfo(
            executable = launcher.executable,
            files = default_files,
            runfiles = runfiles,
        ),
        info,
        output_group,
        NativeExecutableInfo(launcher = launcher, default_files = default_files),
    ]

def _ts_binary_executable_impl(ctx):
    return complete_native_executable(ctx, ctx.label.name + "_launcher")

_BINARY_ATTRS = {
    "entry_point": attr.label(
        doc = "The ts_compile target whose output is the binary entry point, or a single .js/.mjs/.cjs source file to run as-is.",
        allow_files = True,
        mandatory = True,
    ),
    "data": attr.label_list(
        doc = "Extra runfiles: sibling modules a source entry_point imports, fixtures, anything read at runtime. TsInfo targets retain their declared runtime closure and npm bindings, including with a bundled entry.",
        allow_files = True,
    ),
    "entry_file": attr.string(
        doc = "Source filename selecting one entry output. If unset, a sole output, sole local source, or unique local index.js is selected; ambiguous outputs require an explicit entry.",
        default = "",
    ),
    "bundler": attr.label(
        doc = "Optional target providing BundlerInfo. When set, the bundle output is executed. When absent, the entry point .js is run directly.",
        providers = [BundlerInfo],
        default = None,
    ),
    "bundle_name": attr.string(
        doc = "Name for the output bundle file (without extension). Defaults to the rule name. Only meaningful when bundler is set.",
        default = "",
    ),
    "format": attr.string(
        doc = "Output module format: 'esm', 'cjs', 'iife'. Passed to the bundler. Only meaningful when bundler is set.",
        default = "esm",
        values = ["esm", "cjs", "iife"],
    ),
    "sourcemap": attr.bool(
        doc = "Whether to emit a source map alongside the bundle. Only meaningful when bundler is set.",
        default = True,
    ),
    "external": attr.string_list(
        doc = "Module specifiers to mark as external (not bundled). Only meaningful when bundler is set.",
    ),
    "define": attr.string_dict(
        doc = "Global constant replacements. Only meaningful when bundler is set.",
    ),
    "node_modules": attr.label(
        doc = "The importer's `node_modules` target: its links and store " +
              "trees are runfiles, and the directory is on NODE_PATH.",
        providers = [NodeModulesInfo],
    ),
}

_ts_binary_program = rule(
    implementation = _ts_binary_impl,
    executable = True,
    fragments = ["platform"],
    toolchains = LAUNCHER_TOOLCHAINS + [
        TOOLS_TOOLCHAIN_TYPE,
        config_common.toolchain_type(JS_RUNTIME_TOOLCHAIN_TYPE, mandatory = False),
    ],
    attrs = _BINARY_ATTRS | NATIVE_PROGRAM_ATTRS,
)

ts_binary = rule(
    implementation = _ts_binary_executable_impl,
    executable = True,
    toolchains = [TOOLS_TOOLCHAIN_TYPE],
    attrs = _BINARY_ATTRS | NATIVE_EXECUTABLE_ATTRS,
    doc = """Produces an executable binary from a TypeScript entry point.

`bazel run //target` executes the compiled JavaScript using the registered
JS runtime (Node by default). When a bundler target is provided, the bundled
output is executed; otherwise the entry point .js file is run directly.

Example (no bundler — run entry point .js directly):
    ts_binary(
        name = "app",
        entry_point = "//src/app:app",
    )

Example (with bundler — run bundled output; `bundler` is any target returning BundlerInfo):
    ts_binary(
        name = "app",
        entry_point = "//src/app:app",
        bundler = ":bundler",
        format = "cjs",
    )

Example (a plain JavaScript file as the entry point):
    ts_binary(
        name = "generate",
        entry_point = "generate.mjs",
        data = ["helpers.mjs"],
        node_modules = "//:node_modules",
    )
""",
)

def ts_binary_macro(name, **kwargs):
    declare_runnable(name, _ts_binary_program, ts_binary, kwargs)

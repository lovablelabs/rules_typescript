"""Analysis coverage for ts_binary entry shapes and native data inputs."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//tests:runnable_actions.bzl", "runnable_action_aspect", "runnable_actions")
load("//tools/launcher:launcher.bzl", "rlocation_path")
load("//ts:defs.bzl", "BundlerInfo", "TsInfo")
load("//ts/private:providers.bzl", "NpmPackageInfo", "TsTestRunnerInfo", "ts_info")

_RunfilesDataInfo = provider(fields = ["key"])

_RUNFILES_DATA_REPO = Label("@node_test_sources//tests/node_test/analysis:external_sole.test.js")

def _runfiles_runner_impl(ctx):
    runner = ctx.attr.runner[TsTestRunnerInfo]
    data = ctx.file.data

    def launch(test_ctx, test):
        launched = runner.launch(test_ctx, test)
        return struct(
            mode = launched.mode,
            section = launched.section,
            env = launched.env,
            files = launched.files,
            symlinks = launched.symlinks | {"external/" + _RUNFILES_DATA_REPO.repo_name + "/package.json": data},
            transitive_files = launched.transitive_files,
            output_groups = launched.output_groups,
        )

    return [
        TsTestRunnerInfo(
            packages = runner.packages,
            hook = runner.hook,
            es_modules = runner.es_modules,
            supports_source_inputs = runner.supports_source_inputs,
            launch = launch,
        ),
        _RunfilesDataInfo(key = "node_test_sources/package.json"),
    ]

runfiles_runner = rule(
    implementation = _runfiles_runner_impl,
    attrs = {
        "data": attr.label(allow_single_file = True, mandatory = True),
        "runner": attr.label(default = "//ts/runners:node_test", providers = [TsTestRunnerInfo]),
    },
)

def _native_runfiles_fixture_impl(ctx):
    target = ctx.attr.target[DefaultInfo]
    manifest = target.files_to_run.runfiles_manifest
    helper = ctx.attr.helper[DefaultInfo].files_to_run.executable
    mapping = target.files_to_run.repo_mapping_manifest
    runfiles = target.default_runfiles
    record = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(record, json.encode({
        "launcher": rlocation_path(ctx, target.files_to_run.executable),
        "manifest": rlocation_path(ctx, manifest),
        "helper": rlocation_path(ctx, helper),
        "module": rlocation_path(ctx, ctx.file.module),
        "package": rlocation_path(ctx, ctx.attr.package[NpmPackageInfo].store.tree),
        "data": ctx.attr.runner[_RunfilesDataInfo].key if ctx.attr.runner else rlocation_path(ctx, ctx.file.data),
        "original_data": rlocation_path(ctx, ctx.file.data),
    }))
    return [DefaultInfo(
        files = depset([record]),
        runfiles = ctx.runfiles(transitive_files = depset(
            [record, manifest, helper] + ([mapping] if mapping else []) + [link.target_file for link in runfiles.symlinks.to_list() + runfiles.root_symlinks.to_list()],
            transitive = [runfiles.files],
        )),
    )]

native_runfiles_fixture = rule(
    implementation = _native_runfiles_fixture_impl,
    attrs = {
        "target": attr.label(mandatory = True),
        "helper": attr.label(executable = True, cfg = "target", mandatory = True),
        "runner": attr.label(providers = [_RunfilesDataInfo]),
        "module": attr.label(allow_single_file = True, mandatory = True),
        "package": attr.label(providers = [NpmPackageInfo], mandatory = True),
        "data": attr.label(allow_single_file = True, mandatory = True),
    },
)

def _generated_data_entry_impl(ctx):
    entry = ctx.outputs.entry
    ctx.actions.write(entry, "import { answer } from './data-helper.mjs';\nconsole.log(answer);\n")
    ctx.actions.write(ctx.outputs.payload, '{"answer":42}\n')
    return [
        DefaultInfo(files = depset([entry, ctx.outputs.payload])),
        ts_info(js = depset([entry]), label = ctx.label),
    ]

generated_data_entry = rule(
    implementation = _generated_data_entry_impl,
    attrs = {
        "entry": attr.output(mandatory = True),
        "payload": attr.output(mandatory = True),
    },
)

def _analysis_bundler_impl(ctx):
    return [BundlerInfo(
        bundler_binary = ctx.executable._tool,
        config_file = None,
        runtime_deps = depset(),
        use_generated_config = False,
    )]

analysis_bundler = rule(
    implementation = _analysis_bundler_impl,
    attrs = {
        "_tool": attr.label(default = "//ts/tools/tsaction:tsaction", executable = True, cfg = "exec"),
    },
)

def _bundle_borrowed_source_entry_impl(ctx):
    env = analysistest.begin(ctx)
    entry = ctx.attr.entry_point[TsInfo]
    selected = [
        runtime
        for owner in entry.owners.to_list()
        for source, runtime in owner.runtime_files
        if source == ctx.file.source
    ]
    actions = [action for action in runnable_actions(env) if action.mnemonic == "TsBundle"]
    asserts.equals(env, 2, len(entry.js.to_list()), "the compiler retains the local and foreign source outputs")
    asserts.equals(env, 1, len(selected), "the selected source has one compiler output")
    asserts.equals(env, 1, len(actions), "one bundle action consumes the compiler graph")
    if len(selected) == 1 and len(actions) == 1:
        action = actions[0]
        asserts.equals(env, [selected[0].path], [arg for i, arg in enumerate(action.argv) if i and action.argv[i - 1] == "--entry"], "the bundle starts at the selected source output")
        inputs = action.inputs.to_list()
        missing = [
            file.short_path
            for files in [entry.transitive_js, entry.transitive_js_maps, entry.transitive_data]
            for file in files.to_list()
            if file not in inputs
        ]
        asserts.equals(env, [], missing, "entry selection must not remove helpers, maps or data from the bundle inputs")
    return analysistest.end(env)

bundle_borrowed_source_entry_test = analysistest.make(
    _bundle_borrowed_source_entry_impl,
    attrs = {
        "entry_point": attr.label(providers = [TsInfo], mandatory = True),
        "source": attr.label(allow_single_file = True, mandatory = True),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _data_links_impl(ctx):
    env = analysistest.begin(ctx)
    specs = [
        action
        for action in runnable_actions(env)
        if any([file.basename.endswith(".runtime.json") for file in action.outputs.to_list()])
    ]
    asserts.equals(env, 1, len(specs), "one native input wire describes the binary")
    if specs:
        spec = json.decode(specs[0].content)
        inputs = {input["path"]: input for input in spec["inputs"]}
        links = [file for file in ctx.files.fixtures if file.is_symlink]
        asserts.equals(env, ["dangling.js", "dangling.json", "valid.js", "valid.json"], sorted([file.basename for file in links]))
        builders = [action for action in runnable_actions(env) if action.mnemonic == "TsNativeView"]
        outputs = {output.path: output for builder in builders for output in builder.outputs.to_list()}
        for file in links:
            output = outputs.get(spec["root"] + "/" + rlocation_path(ctx, file))
            asserts.true(env, output != None and output.is_symlink, "data link retains its declared kind: " + file.basename)
            asserts.false(env, rlocation_path(ctx, file) in spec["modules"], "data link cannot become a canonical module: " + file.basename)
        file = ctx.file.javascript_data
        asserts.true(env, rlocation_path(ctx, file) in spec["modules"], "regular JavaScript data keeps its module coordinate for every entry shape")
        asserts.equals(env, "alias", inputs[file.path]["kind"], "regular JavaScript data has one canonical module authority")
    return analysistest.end(env)

data_links_test = analysistest.make(
    _data_links_impl,
    attrs = {
        "fixtures": attr.label(mandatory = True),
        "javascript_data": attr.label(allow_single_file = True, mandatory = True),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _fails_with(message):
    def _impl(ctx):
        env = analysistest.begin(ctx)
        asserts.expect_failure(env, message)
        return analysistest.end(env)

    return analysistest.make(_impl, expect_failure = True)

typescript_entry_point_test = _fails_with("is a TypeScript source, which this rule does not compile")
unusable_entry_point_test = _fails_with(
    "does not provide TsInfo and is not a JavaScript file",
)

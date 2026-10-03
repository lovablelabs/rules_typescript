"""Analysis-time proof of what `wrangler_config` stages, and of what it refuses."""

load("@bazel_skylib//lib:paths.bzl", "paths")
load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//tests:runnable_actions.bzl", "runnable_action_aspect", "runnable_actions")
load("//tools/launcher:launcher.bzl", "rlocation_path", "runfiles_scope_paths")
load("//ts/private:providers.bzl", "TsInfo", "TsTestRunnerInfo", "ts_info")

_SOURCE = "tests/workers_nested/wrangler.jsonc"

def _config_runfiles_impl(ctx):
    return [DefaultInfo(files = depset(), runfiles = ctx.runfiles(files = [ctx.file.config]))]

config_runfiles = rule(
    implementation = _config_runfiles_impl,
    attrs = {"config": attr.label(mandatory = True, allow_single_file = True)},
)

def _previous_worker_owner_impl(ctx):
    info = ctx.attr.dep[TsInfo]
    fields = {field: getattr(info, field) for field in dir(info) if field not in ["to_json", "to_proto"]}
    fields["owners"] = depset([
        struct(
            label = str(ctx.label).removeprefix("@@"),
            files = owner.files,
            declarations = owner.declarations,
            type_inputs = owner.type_inputs,
            importers = owner.importers,
        ) if owner.label == str(ctx.attr.dep.label).removeprefix("@@") else owner
        for owner in info.owners.to_list()
    ])
    return [ctx.attr.dep[DefaultInfo], TsInfo(**fields)]

previous_worker_owner = rule(
    implementation = _previous_worker_owner_impl,
    attrs = {"dep": attr.label(providers = [TsInfo], mandatory = True)},
)

def _ordinary_twin_runner_impl(ctx):
    original = ctx.actions.declare_file(ctx.label.name + ".js")
    twin = ctx.actions.declare_file(ctx.label.name + ".es.js")
    ctx.actions.write(original, "module.exports = {};\n")
    ctx.actions.write(twin, "export default {};\n")

    def launch(test_ctx, _test):
        return struct(
            mode = "node",
            section = {"entry": rlocation_path(test_ctx, original)},
            env = {},
            files = [original],
            symlinks = {original.short_path: twin},
            transitive_files = depset(),
            output_groups = {},
        )

    return [
        DefaultInfo(files = depset([original, twin])),
        ts_info(js = depset([original]), transitive_es_twins = depset([(original, twin)]), label = ctx.label),
        TsTestRunnerInfo(packages = [], es_modules = False, supports_source_inputs = False, launch = launch),
    ]

ordinary_twin_runner = rule(implementation = _ordinary_twin_runner_impl)

def _ordinary_twin_impl(ctx):
    env = analysistest.begin(ctx)
    original, twin = ctx.attr.dep[TsInfo].transitive_es_twins.to_list()[0]
    runfiles = analysistest.target_under_test(env)[DefaultInfo].default_runfiles
    path = rlocation_path(ctx, original)
    visible = runfiles_scope_paths(ctx, runfiles, [path])
    asserts.equals(env, original, visible.get(path), "the ordinary module must retain precedence over its unused ES twin")
    asserts.true(env, any([link.target_file == twin for link in runfiles.symlinks.to_list()]), "the fixture must retain the unselected twin link")
    return analysistest.end(env)

ordinary_twin_test = analysistest.make(_ordinary_twin_impl, attrs = {"dep": attr.label(providers = [TsInfo])})

def _wrangler_config_runfiles_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    runfiles = target[DefaultInfo].default_runfiles
    files = runfiles.files.to_list()
    dependency = ctx.attr.dep[TsInfo]
    modules = depset(transitive = [dependency.transitive_js, dependency.transitive_runtime_sources]).to_list()
    data = dependency.transitive_data.to_list()
    pairs = dict([
        (source, runtime)
        for owner in dependency.owners.to_list()
        for source, runtime in getattr(owner, "runtime_files", ())
        if runtime in modules
    ])
    entry = pairs.get(ctx.file.entry)
    if ctx.attr.previous_provider:
        previous = ctx.attr.previous_provider[TsInfo]
        asserts.equals(env, previous.owners.to_list(), dependency.owners.to_list(), "forwarding retains the old-shaped owner record")
        asserts.equals(env, previous.transitive_js.to_list(), dependency.transitive_js.to_list(), "forwarding retains the exact published Files")
        old_owners = [owner for owner in dependency.owners.to_list() if not hasattr(owner, "runtime_files")]
        asserts.equals(env, 1, len(old_owners), "the emitted worker has the previous public owner shape")
        asserts.equals(env, None, entry, "the provider does not invent an entry source/runtime pair")
        entries = [file for file in dependency.js.to_list() if file.short_path == ctx.attr.expected_entry]
        asserts.equals(env, 1, len(entries), "the conventional emitted entry is an actual published File")
        entry = entries[0] if entries else None
        source_modules = [file for file in dependency.sources.to_list() if file.extension == "ts" and not file.basename.endswith(".d.ts")]
        original_paths = [rlocation_path(ctx, file) for file in source_modules]
        originals = runfiles_scope_paths(ctx, runfiles, original_paths)
        for file, path in zip(source_modules, original_paths):
            asserts.false(env, file in files, "source absence must not mask legacy emitted-entry selection")
            asserts.equals(env, None, originals.get(path), "the original TypeScript module has no runtime binding")
        if entry:
            asserts.false(env, entry.is_source, "the old-shaped provider publishes an emitted File")
            asserts.false(env, entry in depset(transitive = [owner.files for owner in old_owners]).to_list(), "legacy owner.files does not enumerate emitted JavaScript")
    else:
        asserts.true(env, entry != None, "the producer records its exact entry source")
    if entry:
        asserts.equals(env, ctx.attr.expected_entry, entry.short_path)
        asserts.equals(env, ctx.attr.expected_entry == ctx.file.entry.short_path, entry == ctx.file.entry)
    assets = {
        (original, coordinate, published): True
        for owner in dependency.owners.to_list()
        for original, coordinate, published in getattr(owner, "asset_files", ())
        if published in data
    }
    configs = {ctx.file.config: True}
    greetings = {}
    for original, coordinate, published in assets:
        if original == ctx.file.config:
            asserts.equals(env, _SOURCE, coordinate)
            configs[published] = True
        if original == ctx.file.greeting:
            asserts.equals(env, ctx.file.greeting.short_path, coordinate)
            greetings[published] = True
    asserts.true(env, bool(greetings), "the producer records the Text module's exact origin")
    producer_config = paths.normalize(paths.join(paths.dirname(ctx.attr.expected_entry), "../wrangler.jsonc"))
    producer_greeting = paths.join(paths.dirname(ctx.attr.expected_entry), "greeting.txt")
    asserts.true(env, producer_config in [file.short_path for file in configs], "the producer publishes the config in its runtime layout")
    asserts.true(env, producer_greeting in [file.short_path for file in greetings], "the Text asset shares the producer's runtime layout")
    caller_config = paths.normalize(paths.join(paths.dirname(ctx.attr.expected_test), "../wrangler.jsonc"))
    config_paths = {file.short_path: True for file in configs}
    config_paths[caller_config] = True
    visible = runfiles_scope_paths(ctx, runfiles, [rlocation_path(ctx, file) for file in modules + greetings.keys()] + [ctx.workspace_name + "/" + path for path in config_paths])
    for file in modules:
        asserts.equals(env, file, visible.get(rlocation_path(ctx, file)), "Workers retains the declared source or emitted module File")
    for file in greetings:
        asserts.equals(env, file, visible.get(rlocation_path(ctx, file)), "the unrelated Text asset keeps its canonical published File")

    patches = [a for a in runnable_actions(env) if a.mnemonic == "WranglerTestConfig"]
    asserts.equals(env, 1, len(patches), "one WranglerTestConfig action")
    if patches:
        prepared = patches[0].outputs.to_list()[0]
        for path in config_paths:
            asserts.equals(env, prepared, visible.get(ctx.workspace_name + "/" + path), "every producer and caller config binding selects the same prepared File: " + path)
        asserts.false(env, prepared in files, "the prepared File is exposed only through the declared config bindings")
        for file in configs:
            asserts.false(env, file in files, "an original config File must not shadow its prepared replacement")
        argv = patches[0].argv
        asserts.equals(env, ctx.file.config.path, argv[argv.index("--config") + 1])
        transported = [json.decode(argv[i + 1]) for i in range(len(argv) - 1) if argv[i] == "--runtime-file"]
        legacy_js = [argv[i + 1] for i in range(len(argv) - 1) if argv[i] == "--runtime-js"]
        legacy_sources = [argv[i + 1] for i in range(len(argv) - 1) if argv[i] == "--runtime-source"]
        if ctx.attr.previous_provider:
            asserts.true(env, ctx.attr.expected_entry in legacy_js, "the old-shaped emitted File reaches conventional entry selection")
            asserts.false(env, ctx.file.entry.short_path in legacy_sources, "the original source is not a published runtime input")
        for source, runtime in pairs.items():
            asserts.false(env, runtime.short_path in legacy_js + legacy_sources, "explicit mappings never become inferred legacy candidates")
            asserts.true(env, [source.short_path, runtime.short_path] in transported, "the patcher receives the producer's exact source/runtime pair")
        asserts.true(env, [ctx.file.test_source.short_path, ctx.attr.expected_test] in transported, "the caller's own runtime placement is independent of the producer's")
        selected_tests = [file for file in files if file.short_path == ctx.attr.expected_test]
        asserts.equals(env, 1, len(selected_tests), "the caller runs the module at its declared placement")
        tree_path = "tests/codegen_tree/compiled"
        asserts.true(
            env,
            [tree_path, tree_path] in transported,
            "an unrelated generated tree must remain a runtime artifact identity",
        )
        asserts.false(env, tree_path in legacy_js + legacy_sources, "opaque directory artifacts do not supply conventional entry candidates")
        asserts.false(
            env,
            tree_path in [f.short_path for f in patches[0].inputs.to_list()],
            "Wrangler config projection must not read unrelated runtime tree contents",
        )

    return analysistest.end(env)

wrangler_config_runfiles_test = analysistest.make(_wrangler_config_runfiles_impl, attrs = {
    "config": attr.label(default = Label("//tests/workers_nested:wrangler.jsonc"), allow_single_file = True),
    "dep": attr.label(providers = [TsInfo]),
    "entry": attr.label(default = Label("//tests/workers_nested:src/index.ts"), allow_single_file = True),
    "expected_entry": attr.string(mandatory = True),
    "expected_test": attr.string(mandatory = True),
    "greeting": attr.label(default = Label("//tests/workers_nested:src/greeting.txt"), allow_single_file = True),
    "previous_provider": attr.label(providers = [TsInfo]),
    "test_source": attr.label(default = Label("//tests/workers_nested/test:worker.test.ts"), allow_single_file = True),
}, extra_target_under_test_aspects = [runnable_action_aspect])

def _fails_with(*messages):
    def _impl(ctx):
        env = analysistest.begin(ctx)
        for message in messages:
            asserts.expect_failure(env, message)
        return analysistest.end(env)

    return analysistest.make(_impl, expect_failure = True)

data_shadow_test = _fails_with(_SOURCE + "' selects", "instead of published runtime File", "_wrangler.jsonc")

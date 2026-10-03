"""The ES-modules emit at analysis: which TsEmit actions a program tsgo emits
and a vitest test register, and what each reads."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//tests:runnable_actions.bzl", "runnable_action_aspect", "runnable_action_label", "runnable_actions")
load("//tools/launcher:launcher.bzl", "rlocation_path", "runfiles_root_path", "runfiles_scope_paths")
load("//ts:defs.bzl", "ts_codegen", "ts_compile", "ts_test")
load("//ts/private:providers.bzl", "TsInfo")

_PKG = "tests/vitest/commonjs"

def _emit_actions(env):
    return [
        action
        for action in runnable_actions(env)
        if action.mnemonic == "TsEmit"
    ]

def _flags(action, prefix):
    return [a for a in action.argv if a.startswith(prefix)]

def _es_twins_impl(ctx):
    env = analysistest.begin(ctx)
    emits = _emit_actions(env)
    asserts.equals(env, 2, len(emits), "TsEmit: the program's and its twins'")
    program = [a for a in emits if "-es_modules" not in a.argv]
    twins = [a for a in emits if "-es_modules" in a.argv]
    asserts.equals(env, 1, len(program), "the program's emit, by module")
    asserts.equals(env, 1, len(twins), "the twins' emit, -es_modules")
    if len(program) != 1 or len(twins) != 1:
        return analysistest.end(env)

    asserts.equals(env, 1, len(_flags(program[0], "-tsgo=")), "tsgo emits")
    asserts.equals(env, [], _flags(twins[0], "-tsgo="), "the twins are oxc's")
    asserts.equals(env, [], _flags(twins[0], "-node_modules="), "no chain")
    out_dirs = _flags(twins[0], "-out_dir=")
    twins_dir = "/" + _PKG + "/commonjs.es"
    asserts.true(
        env,
        len(out_dirs) == 1 and out_dirs[0].endswith(twins_dir),
        "the twins land under <name>.es: " + str(out_dirs),
    )
    asserts.equals(
        env,
        [_PKG + "/commonjs.es/" + f for f in ["helper.js", "vitest.setup.js"]],
        sorted([f.short_path for f in twins[0].outputs.to_list()]),
        "one twin per .ts, at its package-relative path",
    )
    return analysistest.end(env)

es_twins_test = analysistest.make(_es_twins_impl, extra_target_under_test_aspects = [runnable_action_aspect])

def _assert_replacement_identity(ctx, env, target):
    runfiles = target[DefaultInfo].default_runfiles
    pairs = ctx.attr.dep[TsInfo].transitive_es_twins.to_list()
    asserts.true(env, bool(pairs), "the dependency supplies replacements for imported modules")
    visible = runfiles_scope_paths(ctx, runfiles, [rlocation_path(ctx, js) for js, _es in pairs])
    for js, es in pairs:
        asserts.equals(env, ctx.attr.external, js.short_path.startswith("../"), "the fixture's compiler File has the declared repository identity")
        asserts.equals(env, es, visible.get(rlocation_path(ctx, js)), "the runner publishes the exact ES twin at the imported module path")
    if ctx.attr.selected_roots:
        prefix = target.label.package + "/_" + target.label.name + ".vitest/tests/"
        discovery = {js: runfiles_root_path(ctx, prefix + rlocation_path(ctx, js)) for js, _es in pairs}
        visible = runfiles_scope_paths(ctx, runfiles, discovery.values())
        for js, es in pairs:
            asserts.true(env, js not in runfiles.files.to_list(), "ordinary inputs cannot override an explicit runner replacement")
            asserts.equals(env, es, visible.get(discovery[js]), "discovery and imports select the same canonical ES File")
        launchers = [action for action in runnable_actions(env) if any([file.basename == runnable_action_label(env).name + "_test_launcher.json" for file in action.outputs.to_list()])]
        asserts.equals(env, 1, len(launchers), "one launcher config owns runtime materialization")
        if launchers:
            modules = json.decode(launchers[0].content)["runtime_modules"]
            for js, es in pairs:
                asserts.true(env, rlocation_path(ctx, js) in modules, "the original imported coordinate remains canonical")
                asserts.true(env, discovery[js] not in modules and rlocation_path(ctx, es) not in modules, "discovery and twin coordinates cannot create a second module identity")

def _es_modules_emit_impl(ctx):
    env = analysistest.begin(ctx)
    emits = _emit_actions(env)
    asserts.equals(env, 1, len(emits), "TsEmit: the ES-modules emit alone")
    if len(emits) != 1:
        return analysistest.end(env)
    asserts.true(env, "-es_modules" in emits[0].argv, "-es_modules")
    asserts.equals(env, [], _flags(emits[0], "-tsgo="), "oxc's alone")
    target = analysistest.target_under_test(env)
    runfiles = target[DefaultInfo].default_runfiles
    _assert_replacement_identity(ctx, env, target)
    if ctx.file.staged_config:
        links = [link for link in runfiles.symlinks.to_list() if link.target_file == ctx.file.staged_config]
        asserts.equals(env, 1, len(links), "the config has one private runfiles input")
        if links:
            prefix = target.label.package + "/_" + target.label.name + ".vitest/"
            asserts.true(env, links[0].path.startswith(prefix), "the config input remains under the test's private directory")
            asserts.false(env, ".." in links[0].path.split("/"), "an external config cannot traverse the private directory")
            launchers = [action for action in runnable_actions(env) if any([file.basename == runnable_action_label(env).name + "_test_launcher.json" for file in action.outputs.to_list()])]
            asserts.equals(env, 1, len(launchers), "one launcher config names the private staging input")
            if launchers:
                stage = json.decode(launchers[0].content)["vitest"]["stage"]
                asserts.equals(env, rlocation_path(ctx, ctx.file.staged_config), stage.get(ctx.workspace_name + "/" + links[0].path), "private staging retains the external config's runtime coordinate")
    return analysistest.end(env)

es_modules_emit_test = analysistest.make(_es_modules_emit_impl, attrs = {
    "dep": attr.label(providers = [TsInfo]),
    "external": attr.bool(),
    "selected_roots": attr.bool(),
    "staged_config": attr.label(allow_single_file = True),
}, extra_target_under_test_aspects = [runnable_action_aspect])

def _selected_commonjs_impl(ctx):
    env = analysistest.begin(ctx)
    info = ctx.attr.dep[TsInfo]
    asserts.equals(env, ctx.attr.scoped, any([getattr(owner, "runtime_scopes", ()) for owner in info.owners.to_list()]), "the producer's package scope makes a private discovery publication observable")
    _assert_replacement_identity(ctx, env, analysistest.target_under_test(env))
    return analysistest.end(env)

_selected_commonjs_test = analysistest.make(_selected_commonjs_impl, attrs = {
    "dep": attr.label(providers = [TsInfo]),
    "external": attr.bool(),
    "scoped": attr.bool(),
    "selected_roots": attr.bool(default = True),
}, extra_target_under_test_aspects = [runnable_action_aspect])

def _published_javascript_impl(ctx):
    return [DefaultInfo(files = ctx.attr.dep[TsInfo].js)]

_published_javascript = rule(implementation = _published_javascript_impl, attrs = {"dep": attr.label(providers = [TsInfo])})

def _replacement_data_conflict_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "instead of published runtime File")
    asserts.expect_failure(env, "remove or relocate the conflicting data or runfiles entry")
    return analysistest.end(env)

_replacement_data_conflict_test = analysistest.make(_replacement_data_conflict_impl, expect_failure = True)

def selected_commonjs_case(name, scoped):
    ts_codegen(
        name = name + "_inputs",
        srcs = ["external_cjs_source.ts", "helper.ts"] + (["package.json"] if scoped else []),
        outs = [name + "/chosen.test.ts", name + "/helper.ts"] + ([name + "/package.json"] if scoped else []),
        generator = "//ts/tools/tsaction:tsaction",
        args = ["stage", "-out={outs_dir}", "{srcs_dir}/external_cjs_source.ts", "chosen.test.ts", "{srcs_dir}/helper.ts", "helper.ts"] + (["{srcs_dir}/package.json", "package.json"] if scoped else []),
    )
    ts_compile(
        name = name + "_producer",
        srcs = [":" + name + "/chosen.test.ts", ":" + name + "/helper.ts"],
        emit = True,
        node_modules = "//tests/npm:node_modules",
        package_scopes = [":" + name + "/package.json"] if scoped else [],
        tsconfig = ":commonjs_tsconfig",
        deps = ["@npm//:vitest"],
    )
    _published_javascript(name = name + "_runtime", dep = ":" + name + "_producer")
    ts_test(
        name = name + "_test",
        srcs = [":" + name + "_runtime"],
        test_srcs = [":" + name + "_runtime"],
        node_modules = "//tests/npm:node_modules",
        deps = [":" + name + "_producer", "@npm//:vitest"],
    )
    _selected_commonjs_test(name = name + "_cannot_split_selected_identity_test", target_under_test = ":" + name + "_test", dep = ":" + name + "_producer", scoped = scoped)
    if scoped:
        ts_test(
            name = name + "_conflicting_data",
            srcs = [":" + name + "_runtime"],
            test_srcs = [":" + name + "_runtime"],
            data = [":" + name + "_runtime"],
            node_modules = "//tests/npm:node_modules",
            # Only the failure assertion may analyze this contradictory data publication.
            tags = ["manual"],
            deps = [":" + name + "_producer", "@npm//:vitest"],
        )
        _replacement_data_conflict_test(name = name + "_cannot_rewrite_explicit_data_test", target_under_test = ":" + name + "_conflicting_data")

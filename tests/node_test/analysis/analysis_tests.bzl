"""Analysis-time guards on the node:test runner."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//tests:runnable_actions.bzl", "runnable_action_aspect", "runnable_action_label", "runnable_actions", "runnable_repo_mapping")
load("//tools/launcher:launcher.bzl", "LAUNCHER_TOOLCHAINS", "declare_launcher", "rlocation_path", "runfiles_scope_paths")
load("//ts:defs.bzl", "ts_binary", "ts_compile", "ts_test")
load("//ts/private:providers.bzl", "TsInfo", "TsTestRunnerInfo")
load("//ts/private:toolchain.bzl", "TOOLS_TOOLCHAIN_TYPE")

def _fails_with(*messages):
    def _impl(ctx):
        env = analysistest.begin(ctx)
        for message in messages:
            asserts.expect_failure(env, message)
        if ctx.attr.runtime_path:
            asserts.expect_failure(env, ctx.attr.runtime_path)
        return analysistest.end(env)

    return analysistest.make(_impl, expect_failure = True, attrs = {"runtime_path": attr.string()})

vitest_attr_test = _fails_with(
    "the node:test runner reads none of config, coverage_provider, " +
    "wrangler_config.",
    "configures vitest, which this target does not run",
)

undeclared_test_root_test = _fails_with("is not in srcs", "Did you mean")
empty_test_root_test = _fails_with("test_srcs selects no executable program inputs", "Did you mean")
config_scope_change_test = _fails_with("config staging changes the published runtime package scope", "Did you mean")

def _private_scope_alias_impl(ctx):
    env = analysistest.begin(ctx)
    scope = ctx.file.scope
    aliases = [runtime for owner in ctx.attr.producer[TsInfo].owners.to_list() for source, runtime in getattr(owner, "runtime_scopes", ()) if source == scope]
    asserts.equals(env, 1, len(aliases))
    actions = runnable_actions(env)
    builders = [action for action in actions if action.mnemonic == "TsNativeView"]
    specs = [action for action in actions if any([file.basename.endswith(".runtime.json") for file in action.outputs.to_list()])]
    asserts.equals(env, 1, len(builders))
    asserts.equals(env, 1, len(specs))
    if aliases and builders and specs:
        alias = aliases[0]
        asserts.true(env, alias != scope, "the canonical scope has no separate public runfiles coordinate")
        spec = json.decode(specs[0].content)
        inputs = {entry["path"]: entry for entry in spec["inputs"]}
        asserts.equals(env, alias.path, spec["entries"].get(rlocation_path(ctx, scope)), "the admitted scope retains its producer's alias")
        asserts.false(env, scope.path in spec["entries"].values(), "canonical input retention cannot introduce another public entry")
        asserts.true(env, scope in builders[0].inputs.to_list(), "the native materializer retains the exact private canonical input")
        asserts.true(env, scope.path in inputs, "the canonical scope needs an internal authority")
        if scope.path in inputs:
            asserts.equals(env, inputs[scope.path]["output"], inputs[alias.path].get("target"), "the alias resolves to the one canonical input authority")
    return analysistest.end(env)

private_scope_alias_test = analysistest.make(
    _private_scope_alias_impl,
    attrs = {
        "producer": attr.label(providers = [TsInfo]),
        "scope": attr.label(allow_single_file = True),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _private_module_aliases_impl(ctx):
    aliases = [ctx.actions.declare_file(ctx.label.name + "/" + name + ".mjs") for name in ["first", "second"]]
    for alias in aliases:
        ctx.actions.symlink(output = alias, target_file = ctx.file.source)
    launcher = declare_launcher(
        ctx,
        {
            "mode": "node",
            "workspace": ctx.workspace_name,
            "node": {"entry": rlocation_path(ctx, aliases[0])},
            "runtime_modules": [rlocation_path(ctx, alias) for alias in aliases],
        },
        runfiles = ctx.runfiles(files = aliases),
        canonical_links = [(alias, ctx.file.source) for alias in aliases],
    )
    return [DefaultInfo(files = depset(launcher.files))]

private_module_aliases = rule(
    implementation = _private_module_aliases_impl,
    attrs = {"source": attr.label(allow_single_file = True)},
    fragments = ["platform"],
    toolchains = [TOOLS_TOOLCHAIN_TYPE] + LAUNCHER_TOOLCHAINS,
)

private_module_alias_conflict_test = _fails_with("reaches multiple canonical module paths", "Did you mean")

def _selected_runtime_roots_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    source_entries = ctx.files.expected_source_entries + [file for producer in ctx.attr.published_entries for file in producer[TsInfo].js.to_list()]
    published = {source: source for source in source_entries + ctx.files.runtime_companions}
    if ctx.attr.emitted_external:
        for source in published.keys():
            outputs = [output for action in runnable_actions(env) if action.mnemonic != "TsNativeView" and source in action.inputs.to_list() for output in action.outputs.to_list() if output.basename == source.basename]
            asserts.equals(env, 1, len(outputs), "one producer projects the exact external input: " + source.short_path)
            if outputs:
                output = outputs[0]
                asserts.equals(env, target.label.package + "/external/" + source.short_path[3:], output.short_path, "external repository identity survives the common output layout")
                published[source] = output
    test_files_path = target[DefaultInfo].files_to_run.executable.dirname + "/" + runnable_action_label(env).name + "_test_files.txt"
    writers = [
        action
        for action in runnable_actions(env)
        if any([file.path == test_files_path for file in action.outputs.to_list()])
    ]
    asserts.equals(env, 1, len(writers), "one action writes the selected execution roots")
    actual = []
    if writers:
        expected = [ctx.workspace_name + "/" + name for name in ctx.attr.expected_entries]
        expected.extend([rlocation_path(ctx, published[file]) for file in source_entries])
        actual = [line for line in (writers[0].content or "").split("\n") if line]
        asserts.equals(env, sorted(expected), sorted(actual), "every selected root reaches its imported runtime path")
    launchers = [
        action
        for action in runnable_actions(env)
        if any([file.basename == runnable_action_label(env).name + "_test_launcher.json" for file in action.outputs.to_list()])
    ]
    asserts.equals(env, 1, len(launchers), "one launcher config selects the runner")
    if launchers:
        config = json.decode(launchers[0].content)
        modules = config.get("runtime_modules", [])
        if config["mode"] in ["node", "node_test"]:
            anchor = rlocation_path(ctx, launchers[0].outputs.to_list()[0])
            asserts.equals(env, anchor, config.get("native_view_anchor"), "native execution locates the canonical config File")
            builders = [action for action in runnable_actions(env) if action.mnemonic == "TsNativeView"]
            specs = [action for action in runnable_actions(env) if any([file.basename.endswith(".runtime.json") for file in action.outputs.to_list()])]
            asserts.equals(env, 1, len(builders), "one action owns the composed native runtime outputs")
            asserts.equals(env, 1, len(specs), "one admitted input map drives the native runtime action")
            if builders and specs:
                spec = json.decode(specs[0].content)
                modules = spec["modules"]
                inputs = builders[0].inputs.to_list()
                outputs = {file.path: file for file in builders[0].outputs.to_list()}
                for source, runtime in published.items():
                    path = rlocation_path(ctx, runtime)
                    asserts.true(env, runtime in inputs, "native materializer consumes the selected File: " + source.short_path)
                    asserts.equals(env, runtime.path, spec["entries"].get(path), "native materializer retains exact admitted File provenance")
                    view_path = spec["root"] + "/" + path
                    output = outputs.get(view_path)
                    trees = [tree for tree in outputs.values() if tree.is_directory and view_path.startswith(tree.path + "/")]
                    asserts.true(env, (output != None and not output.is_directory and not output.is_symlink) or (output == None and len(trees) == 1), "selected module has one declared regular runtime output or link-free tree")
        for path in actual + [rlocation_path(ctx, published[file]) for file in ctx.files.runtime_companions]:
            asserts.true(env, path in modules, "selected roots and imported JSON retain their staged identities: " + path)

    visible = runfiles_scope_paths(ctx, target[DefaultInfo].default_runfiles, actual + [rlocation_path(ctx, published[file]) for file in ctx.files.runtime_companions])
    for source in source_entries + ctx.files.runtime_companions:
        runtime = published[source]
        asserts.equals(env, runtime, visible.get(rlocation_path(ctx, runtime)), "selected roots and imported companions retain the exact published File: " + source.short_path)
    if ctx.attr.vitest_discovery:
        links = {link.path: link.target_file for link in target[DefaultInfo].default_runfiles.symlinks.to_list()}
        for source in source_entries:
            path = target.label.package + "/_" + target.label.name + ".vitest/tests/" + rlocation_path(ctx, source)
            asserts.equals(env, published[source], links.get(path), "modern and prior-wire roots retain their requested-input discovery link")
    if ctx.file.competing_file:
        competitor = ctx.file.competing_file
        path = rlocation_path(ctx, competitor)
        outputs = [output for action in runnable_actions(env) if action.mnemonic != "TsNativeView" for output in action.outputs.to_list() if output.basename == competitor.basename]
        asserts.equals(env, 1, len(outputs), "the selected source has one canonical emitted output")
        if outputs:
            runtime = outputs[0]
            runtime_path = rlocation_path(ctx, runtime)
            asserts.true(env, runtime != competitor, "ordinary source data and emitted root have distinct File identities")
            asserts.equals(env, [runtime_path], actual, "the test-files list selects the producer's canonical output")
            asserts.equals(env, runtime, visible.get(runtime_path), "selected canonical output survives ordinary data admission")
            asserts.equals(env, competitor, runfiles_scope_paths(ctx, target[DefaultInfo].default_runfiles, [path]).get(path), "ordinary data keeps its own original path")
            replaced = target[DefaultInfo].default_runfiles.merge(ctx.runfiles(files = [competitor]))
            asserts.equals(env, runtime, runfiles_scope_paths(ctx, replaced, [runtime_path]).get(runtime_path), "merging source data cannot replace a different canonical output")
    return analysistest.end(env)

selected_runtime_roots_test = analysistest.make(
    _selected_runtime_roots_impl,
    attrs = {
        "expected_entries": attr.string_list(),
        "expected_source_entries": attr.label_list(allow_files = True),
        "runtime_companions": attr.label_list(allow_files = True),
        "competing_file": attr.label(allow_single_file = True),
        "emitted_external": attr.bool(),
        "published_entries": attr.label_list(providers = [TsInfo]),
        "vitest_discovery": attr.bool(),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _native_runfiles_mapping_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    public_actions = analysistest.target_actions(env)
    actions = runnable_actions(env)
    mapping = runnable_repo_mapping(env)
    copies = [action for action in public_actions if action.mnemonic == "TsRunfilesMapping"]
    asserts.true(env, mapping != None, "the completed producer supplies Bazel's repository mapping")
    asserts.equals(env, 1, len(copies), "one public action retains the producer's exact mapping File")
    if copies:
        asserts.true(env, mapping in copies[0].inputs.to_list(), "mapping rows come from the completed producer File")
        asserts.true(env, mapping.path in copies[0].argv, "the mapping copy consumes that exact Bazel artifact")
        copied = copies[0].outputs.to_list()
        asserts.equals(env, ["_repo_mapping"], [file.basename for file in copied])
        asserts.true(env, all([file in target[DefaultInfo].default_runfiles.files.to_list() for file in copied]), "mapping survives executable-as-data composition")
    configs = [action for action in actions if any([file.basename.endswith("_launcher.json") for file in action.outputs.to_list()])]
    specs = [action for action in actions if any([file.basename.endswith(".runtime.json") for file in action.outputs.to_list()])]
    asserts.equals(env, 1, len(configs), "one producer config selects the native view")
    asserts.equals(env, 1, len(specs), "one producer owns native File admission")
    if configs and specs:
        config = json.decode(configs[0].content)
        spec = json.decode(specs[0].content)
        suffix = "_test_launcher" if config["mode"] == "node_test" else "_launcher"
        asserts.equals(env, target.label.name + suffix, target[DefaultInfo].files_to_run.executable.basename, "public executable keeps its original name")
        asserts.equals(env, str(target.label), config["label"], "launcher metadata names the public target")
        asserts.equals(env, rlocation_path(ctx, configs[0].outputs.to_list()[0]), config["native_view_anchor"], "the view stays anchored to its action-owning config")
        if copies:
            asserts.equals(env, spec["root"] + "/_repo_mapping", copies[0].outputs.to_list()[0].path, "child lookup reads the completed producer's mapping inside its own view")
        for file in [ctx.file.entry, ctx.file.module]:
            asserts.equals(env, file.path, spec["entries"].get(rlocation_path(ctx, file)), "original module File provenance survives public completion")
        runtime = config["runtime"]
        retained = [file for file in target[DefaultInfo].default_runfiles.files.to_list() if rlocation_path(ctx, file) == runtime]
        asserts.equals(env, 1, len(retained), "the original runtime executable remains a declared runfile")
        if retained:
            asserts.equals(env, retained[0].path, spec["entries"].get(runtime), "view lookup retains declared runtime bytes through the same native materializer")
            inputs = [item for item in spec["inputs"] if item["path"] == retained[0].path]
            asserts.equals(env, [("placed", spec["root"] + "/" + runtime)], [(item["kind"], item.get("target")) for item in inputs], "the runtime authority relocates with the application view")
        if config["mode"] == "node_test":
            checks = [action for action in actions if action.mnemonic == "TsgoCheck"]
            asserts.equals(env, 1, len(checks), "one private program owns test validation")
            asserts.true(env, InstrumentedFilesInfo in target, "coverage is published by the public test")
            for check in checks:
                asserts.true(env, all([file in target[OutputGroupInfo]._validation.to_list() for file in check.outputs.to_list()]), "the public test forwards the private check")
        else:
            asserts.equals(env, [ctx.file.entry], target[TsInfo].js.to_list(), "the public binary forwards the original entry provider")
            asserts.equals(env, target[TsInfo].transitive_js.to_list(), target[OutputGroupInfo].js_tree.to_list(), "the public output group retains the producer's module closure")
    asserts.equals(env, 1, len([action for action in actions if action.mnemonic == "TsNativeView"]), "one producer owns the native view")
    asserts.equals(env, [], [action.mnemonic for action in public_actions if action.mnemonic in ["TsConfig", "TsgoCheck", "TsNativeView"]], "public completion cannot compile or materialize the program again")
    return analysistest.end(env)

native_runfiles_mapping_test = analysistest.make(
    _native_runfiles_mapping_impl,
    attrs = {
        "entry": attr.label(allow_single_file = True, mandatory = True),
        "module": attr.label(allow_single_file = True, mandatory = True),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _runtime_scope_inputs_impl(ctx):
    runtime = ctx.outputs.src.basename[:-3] + ctx.attr.runtime_extension
    annotation = ": string" if ctx.outputs.src.extension == "ts" else ""
    check = "const moduleURL" + annotation + " = import.meta.url;\nif (!moduleURL.endsWith('/nested/" + runtime + "')) throw new Error(moduleURL);\n"
    if ctx.attr.vitest:
        check = "import { it } from 'vitest';\nit('selected module retains its execution path', () => {\n" + check + "});\n"
    ctx.actions.write(ctx.outputs.src, check)
    ctx.actions.write(ctx.outputs.manifest, '{"type":"commonjs"}\n')
    files = [ctx.outputs.src, ctx.outputs.manifest]
    if ctx.outputs.scope:
        ctx.actions.write(ctx.outputs.scope, '{"type":"module"}\n')
        files.append(ctx.outputs.scope)
    if ctx.outputs.json_src:
        ctx.actions.write(ctx.outputs.json_src, "{}\n")
        files.append(ctx.outputs.json_src)
    if ctx.outputs.consumer:
        ctx.actions.write(ctx.outputs.consumer, "import './nested/" + runtime + "';\n")
        files.append(ctx.outputs.consumer)
    return [DefaultInfo(files = depset(files))]

_runtime_scope_inputs = rule(
    implementation = _runtime_scope_inputs_impl,
    attrs = {name: attr.output(mandatory = True) for name in ["src", "manifest"]} | {
        "scope": attr.output(),
        "json_src": attr.output(),
        "consumer": attr.output(),
        "vitest": attr.bool(),
        "runtime_extension": attr.string(default = ".js"),
    },
)

def _runtime_scope_data_impl(ctx):
    manifest = ctx.file.manifest
    placement = ctx.attr.placement
    path = "package.json" if placement == "workspace_root" else ctx.attr.path
    files = []
    if placement in ["tree", "opaque_link"]:
        tree = ctx.actions.declare_directory(ctx.label.name + ".tree")
        ctx.actions.run(
            executable = ctx.executable._stage,
            arguments = ["stage", "-out=" + tree.path, manifest.path, "package.json"],
            inputs = [manifest],
            outputs = [tree],
        )
        manifest = tree
        if placement == "opaque_link":
            manifest = ctx.actions.declare_symlink(ctx.label.name + ".link")
            ctx.actions.symlink(output = manifest, target_path = tree.basename)
            files = [tree]
    runfiles = ctx.runfiles(
        files = [manifest] if placement == "files" else files,
        symlinks = {path: manifest} if placement in ["symlink", "tree", "opaque_link", "workspace_root"] else {},
        root_symlinks = {ctx.workspace_name + "/" + path: manifest} if placement == "root_symlink" else {},
    )
    return [DefaultInfo(files = depset(), runfiles = runfiles)]

_runtime_scope_data = rule(
    implementation = _runtime_scope_data_impl,
    attrs = {
        "manifest": attr.label(allow_single_file = True),
        "path": attr.string(),
        "placement": attr.string(),
        "_stage": attr.label(default = "//ts/tools/tsaction:tsaction", executable = True, cfg = "exec"),
    },
)

def _runtime_scope_runner_impl(ctx):
    manifest = ctx.file.manifest
    path = ctx.label.package + "/" + ctx.attr.path
    direct = ctx.attr.direct
    preserve = ctx.attr.preserve

    def launch(test_ctx, test):
        files = []
        if direct:
            section = {"entry": rlocation_path(test_ctx, test.entry_points[0])}
        else:
            test_files = test_ctx.actions.declare_file(test_ctx.label.name + ".custom_tests.txt")
            test_ctx.actions.write(test_files, "\n".join([rlocation_path(test_ctx, file) for file in test.entry_points]) + "\n")
            files.append(test_files)
            section = {"test_files_list": rlocation_path(test_ctx, test_files)}
        return struct(
            mode = "node" if direct else "node_test",
            section = section,
            env = {"NODE_OPTIONS": "--preserve-symlinks-main"} if preserve else {},
            files = files,
            symlinks = {path: manifest},
            transitive_files = depset(transitive = [test.transitive_js] + test.runtime_data_sets),
            output_groups = {},
        )

    return [TsTestRunnerInfo(packages = [], es_modules = False, supports_source_inputs = False, launch = launch)]

_runtime_scope_runner = rule(
    implementation = _runtime_scope_runner_impl,
    attrs = {"manifest": attr.label(allow_single_file = True), "path": attr.string(), "direct": attr.bool(), "preserve": attr.bool()},
)

_runtime_scope_shadow_test = _fails_with("is shadowed by runtime manifest", "Did you mean")
_runtime_scope_injected_test = _fails_with("has no source package scope but acquires runtime manifest", "Did you mean")
_runtime_scope_directory_test = _fails_with("beneath opaque runtime", "Did you mean")

def _runtime_scope_accepted_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    files = [file for file in runfiles_scope_paths(ctx, target[DefaultInfo].default_runfiles).values() if file != None]
    for suffix in ctx.attr.manifests:
        asserts.true(env, any([file.short_path.endswith("/" + suffix) for file in files]), "accepted runtime retains " + suffix)
    if ctx.file.json_module:
        path = rlocation_path(ctx, ctx.file.json_module)
        visible = runfiles_scope_paths(ctx, target[DefaultInfo].default_runfiles, [path])
        asserts.equals(env, ctx.file.json_module, visible.get(path), "JSON retains its declared File beneath runtime-only package metadata")
    if ctx.file.staged_scope:
        launchers = [a for a in runnable_actions(env) if any([f.basename.endswith("_test_launcher.json") for f in a.outputs.to_list()])]
        asserts.equals(env, 1, len(launchers))
        if launchers:
            config = json.decode(launchers[0].content)
            stage = config["vitest"]["stage"]
            scope = rlocation_path(ctx, ctx.file.staged_scope)
            visible = runfiles_scope_paths(ctx, target[DefaultInfo].default_runfiles, stage.keys())
            sources = [source for source, destination in stage.items() if destination == scope]
            asserts.equals(env, 1, len(sources), "the config must add a scope beside the JSON module")
            if sources:
                asserts.equals(env, ctx.file.staged_scope, visible.get(sources[0]), "config staging retains the declared scope File")
            asserts.true(env, rlocation_path(ctx, ctx.file.json_module) in config["runtime_modules"], "JSON still needs logical-path materialization")
    if ctx.attr.directory:
        links = target[DefaultInfo].default_runfiles.symlinks.to_list()
        asserts.true(env, any([link.path.endswith("/" + ctx.attr.directory) and link.target_file.is_directory for link in links]), "unrelated runtime directory survives")
    if ctx.file.root_alias:
        scopes = runfiles_scope_paths(ctx, target[DefaultInfo].default_runfiles)
        asserts.equals(env, ctx.file.root_alias, scopes.get(ctx.workspace_name + "/package.json"), "workspace-root scope retains its aliased File identity")
    if ctx.file.scope_module:
        path = rlocation_path(ctx, ctx.file.scope_module)[:-3] + ctx.attr.runtime_extension
        visible = runfiles_scope_paths(ctx, target[DefaultInfo].default_runfiles, [path])
        asserts.true(env, visible.get(path) != None, "the selected source or emitted module retains its runtime path")
        if ctx.file.expected_scope:
            asserts.equals(env, ctx.file.expected_scope, visible.get(rlocation_path(ctx, ctx.file.expected_scope)), "the selected module retains the same metadata File")
    return analysistest.end(env)

runtime_scope_accepted_test = analysistest.make(
    _runtime_scope_accepted_impl,
    attrs = {"manifests": attr.string_list(), "directory": attr.string(), "root_alias": attr.label(allow_single_file = True), "json_module": attr.label(allow_single_file = True), "staged_scope": attr.label(allow_single_file = True), "scope_module": attr.label(allow_single_file = True), "expected_scope": attr.label(allow_single_file = True), "runtime_extension": attr.string()},
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def runtime_scope_case(name, placement, shadowed = True, binary = False, scoped = True, json_module = False, vitest = False, emit = True, dependency = False, source_scope = None):
    src = name + ("/nested/check.test.ts" if vitest else "/nested/check.ts" if emit else "/nested/check.js")
    scope = name + "/package.json"
    extra = name + ("/nested/package.json" if shadowed else "/unrelated/package.json")
    manifest = extra if placement in ["direct", "files"] else name + "/fixture.json"
    json_src = name + "/unrelated/value.json" if json_module else None
    consumer = name + "/consumer.test.ts" if dependency else None
    runtime_extension = ".ts" if vitest and not emit and not dependency else ".js"
    _runtime_scope_inputs(name = name + "_inputs", src = src, scope = None if source_scope else scope, manifest = manifest, json_src = json_src, consumer = consumer, vitest = vitest, runtime_extension = runtime_extension)
    runner = "//ts/runners:vitest" if vitest else "//ts/runners:node_test"
    data = []
    if placement == "direct":
        data = [":" + manifest]
    elif placement == "same_file":
        data = [":" + scope]
    elif placement in ["runner", "direct_runner", "preserving_runner"]:
        runner = ":" + name + "_runner"
        _runtime_scope_runner(name = name + "_runner", manifest = ":" + manifest, path = extra, direct = placement != "runner", preserve = placement == "preserving_runner")
    elif placement != "none":
        _runtime_scope_data(
            name = name + "_data",
            manifest = ":" + manifest,
            path = native.package_name() + "/" + (name + ("/nested" if shadowed else "/unrelated") if placement in ["tree", "opaque_link"] else extra),
            placement = placement,
        )
        data = [":" + name + "_data"]

    # These subjects intentionally fail analysis; their analysistest wrappers are the runnable tests.
    tags = ["manual"] if shadowed else []
    package_scopes = [source_scope or ":" + scope] if scoped else []
    srcs = [":" + src] + ([":" + json_src] if json_src else [])
    deps = ["@npm//:vitest"] if vitest else []
    node_modules = "//tests/npm:node_modules" if vitest else None
    if dependency:
        ts_compile(name = name + "_dependency", srcs = srcs, package_scopes = package_scopes, emit = True, node_modules = node_modules, deps = deps)
        srcs = [":" + consumer]
        deps = deps + [":" + name + "_dependency"]
        package_scopes = []
    if binary:
        ts_compile(name = name + "_entry", srcs = srcs, package_scopes = package_scopes, emit = True)
        ts_binary(name = name + "_target", entry_point = ":" + name + "_entry", data = data, tags = tags)
    else:
        ts_test(name = name + "_target", srcs = srcs, package_scopes = package_scopes, data = data, emit = emit, runner = runner, tags = tags, node_modules = node_modules, deps = deps)
    if shadowed:
        test = _runtime_scope_shadow_test if scoped else _runtime_scope_injected_test
        if placement in ["tree", "opaque_link"]:
            test = _runtime_scope_directory_test
        test(name = name + "_test", target_under_test = ":" + name + "_target")
    else:
        runtime_scope_accepted_test(
            name = name + "_test",
            target_under_test = ":" + name + "_target",
            manifests = ([scope] if scoped else []) + ([manifest] if placement not in ["tree", "workspace_root", "none", "same_file"] else []),
            directory = name + "/unrelated" if placement == "tree" else "",
            root_alias = ":" + manifest if placement == "workspace_root" else None,
            json_module = ":" + json_src if json_src else None,
            scope_module = ":" + src if vitest or source_scope else None,
            expected_scope = (source_scope or ":" + scope) if scoped and (vitest or source_scope) else None,
            runtime_extension = runtime_extension,
        )

def _module_alias_inputs_impl(ctx):
    ctx.actions.write(ctx.outputs.src, 'throw new Error("selected TypeScript root must fail");\n')
    ctx.actions.write(ctx.outputs.shadow, 'console.log("stale JavaScript must not replace the selected root");\n')
    return [DefaultInfo(files = depset([ctx.outputs.src, ctx.outputs.shadow]))]

module_alias_inputs = rule(
    implementation = _module_alias_inputs_impl,
    attrs = {name: attr.output(mandatory = True) for name in ["src", "shadow"]},
)

def _data_symlink_inputs_impl(ctx):
    payload = ctx.actions.declare_file(ctx.label.name + "/payload.txt")
    ctx.actions.write(payload, "linked data\n")
    files = [payload]
    for extension in ["json", "js"]:
        for name, target in [("valid", "payload.txt"), ("dangling", "absent.txt")]:
            link = ctx.actions.declare_symlink(ctx.label.name + "/" + name + "." + extension)
            ctx.actions.symlink(output = link, target_path = target)
            files.append(link)
    return [DefaultInfo(files = depset(files), runfiles = ctx.runfiles(files = files))]

data_symlink_inputs = rule(implementation = _data_symlink_inputs_impl)

runtime_module_collision_test = _fails_with("runtime path", "instead of published runtime File", "Did you mean")
_runtime_module_directory_test = _fails_with("beneath opaque runtime entry", "Did you mean")

def runtime_module_alias_case(name, placement, same_file = False):
    stem = "same_file" if same_file else placement
    directory = "tests/node_test/foreign_alias/" + stem if same_file else native.package_name() + "/foreign_alias/" + stem
    path = directory + "/selected.test.js"
    shadow = "//tests/node_test/foreign_alias:" + stem + "/selected.test.js"
    src = shadow if same_file else "//tests/node_test/foreign_alias:" + stem + "/selected.test.ts"
    if placement == "direct":
        data = [shadow]
    else:
        _runtime_scope_data(
            name = name + "_data",
            manifest = shadow,
            path = directory if placement in ["tree", "opaque_link", "root_ancestor"] else path,
            placement = "root_symlink" if placement == "root_ancestor" else placement,
        )
        data = [":" + name + "_data"]

    # The emitted subjects throw if executed; only their analysis wrappers run.
    ts_test(
        name = name + "_target",
        srcs = [src],
        data = data,
        emit = not same_file,
        runner = "//ts/runners:node_test",
        tags = [] if same_file else ["manual"],
    )
    if same_file:
        selected_runtime_roots_test(
            name = name + "_test",
            expected_source_entries = [src],
            target_under_test = ":" + name + "_target",
        )
    elif placement in ["direct", "files"]:
        selected_runtime_roots_test(
            name = name + "_test",
            expected_entries = [path],
            competing_file = shadow,
            target_under_test = ":" + name + "_target",
        )
    else:
        test = _runtime_module_directory_test if placement in ["tree", "opaque_link", "root_ancestor"] else runtime_module_collision_test
        test(name = name + "_test", target_under_test = ":" + name + "_target", runtime_path = path)

def _runfiles_module_projection_impl(ctx):
    env = analysistest.begin(ctx)
    expected = ctx.file.expected
    for carrier in ["symlink", "file", "empty", "root"]:
        for file in [expected, ctx.file.other]:
            path = file.short_path
            rooted = rlocation_path(ctx, file)
            for ancestor in [False, True]:
                module = path + "/child.js" if ancestor else path
                requested = rooted + "/child.js" if ancestor else rooted
                links = [struct(path = module, target_file = expected)]
                if carrier == "symlink":
                    links.append(struct(path = path, target_file = file))
                runfiles = struct(
                    symlinks = depset(links),
                    files = depset([file] if carrier == "file" else []),
                    empty_filenames = depset([path] if carrier == "empty" else []),
                    root_symlinks = depset([struct(path = rooted, target_file = file)] if carrier == "root" else []),
                )
                visible = runfiles_scope_paths(ctx, runfiles, [requested])
                asserts.true(env, rooted in visible, "demanded module and ancestor occupants survive projection: " + carrier)
                asserts.equals(env, None if carrier == "empty" else file, visible.get(rooted), "Bazel carrier precedence determines File identity: " + carrier)
    return analysistest.end(env)

runfiles_module_projection_test = analysistest.make(
    _runfiles_module_projection_impl,
    attrs = {name: attr.label(allow_single_file = True) for name in ["expected", "other"]},
)

def _binary_module_identity_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    files = target[TsInfo].transitive_js.to_list()
    visible = runfiles_scope_paths(ctx, target[DefaultInfo].default_runfiles, [rlocation_path(ctx, file) for file in files])
    for file in files:
        asserts.equals(env, file, visible.get(rlocation_path(ctx, file)), "repeated data selects the published module File")
    asserts.true(env, ctx.file.source in files, "source-mode publication retains the original module File")
    return analysistest.end(env)

binary_module_identity_test = analysistest.make(
    _binary_module_identity_impl,
    attrs = {"source": attr.label(allow_single_file = True)},
)

def _runtime_source_identity_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    actions = runnable_actions(env)
    staged = [file for action in actions if action.mnemonic == "TsStage" for file in action.outputs.to_list()]
    runtime = {file.short_path: file for file in staged + ctx.attr.dep[TsInfo].js.to_list()}
    runfiles = target[DefaultInfo].default_runfiles
    paths = [rlocation_path(ctx, file) for file in ctx.files.copied_sources + ctx.files.retained_sources]
    visible = runfiles_scope_paths(ctx, runfiles, paths)
    files = runfiles.files.to_list()
    for source in ctx.files.copied_sources:
        asserts.true(env, source.is_source, "the regression uses authored inputs that require staging")
        expected = runtime.get(source.short_path)
        asserts.true(env, expected != None and expected != source, "compiler stages a distinct runtime File at the source path")
        asserts.equals(env, [expected], [file for file in files if file.short_path == source.short_path], "one compiler-owned File occupies the runtime path")
        asserts.equals(env, expected, visible.get(rlocation_path(ctx, source)), "the runtime projection selects the staged File")
    for source in ctx.files.retained_sources:
        asserts.equals(env, [source], [file for file in files if file.short_path == source.short_path], "one original File occupies the unchanged runtime path")
        asserts.equals(env, source, visible.get(rlocation_path(ctx, source)), "the runtime retains the exact selected source File")
    test_files_path = target[DefaultInfo].files_to_run.executable.dirname + "/" + runnable_action_label(env).name + "_test_files.txt"
    lists = [action for action in actions if any([file.path == test_files_path for file in action.outputs.to_list()])]
    asserts.equals(env, 1, len(lists), "one entry list selects the runtime roots")
    if lists:
        asserts.equals(env, sorted([rlocation_path(ctx, file) for file in ctx.files.entries]), sorted([line for line in (lists[0].content or "").split("\n") if line]), "source projection does not change test selection")
    if ctx.attr.vitest:
        configs = [action for action in actions if any([file.short_path.endswith("/_" + target.label.name + ".vitest/config.mjs") for file in action.outputs.to_list()])]
        asserts.equals(env, 1, len(configs), "one config supplies Vitest's existing probes")
        if configs:
            probe = ctx.files.retained_sources[0].short_path if ctx.files.retained_sources else ""
            asserts.true(env, "const SOURCE_PROBE = " + json.encode(probe) + ";" in configs[0].content, "only retained authored Files establish the source root")
            asserts.true(env, "const BIN_PROBE = " + json.encode(ctx.files.entries[0].short_path) + ";" in configs[0].content, "the selected entry retains the bin-root probe")
    return analysistest.end(env)

runtime_source_identity_test = analysistest.make(
    _runtime_source_identity_impl,
    attrs = {
        "copied_sources": attr.label_list(allow_files = True),
        "retained_sources": attr.label_list(allow_files = True),
        "entries": attr.label_list(allow_files = True),
        "dep": attr.label(providers = [TsInfo]),
        "vitest": attr.bool(),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _canonical_alias_data_impl(ctx):
    source = ctx.attr.source[TsInfo].js.to_list()[0]
    output = ctx.actions.declare_file(ctx.label.name + ".cjs")
    ctx.actions.run(
        executable = ctx.executable._stage,
        arguments = ["stage", "-out=" + output.dirname, source.path, output.basename],
        inputs = [source],
        outputs = [output],
    )
    return [DefaultInfo(files = depset([output]))]

canonical_alias_data = rule(
    implementation = _canonical_alias_data_impl,
    attrs = {
        "source": attr.label(providers = [TsInfo], mandatory = True),
        "_stage": attr.label(default = "//ts/tools/tsaction:tsaction", executable = True, cfg = "exec"),
    },
)

def _vitest_canonical_alias_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    canonical = ctx.attr.canonical[TsInfo].js.to_list()[0]
    actions = runnable_actions(env)
    aliases = [
        output
        for action in actions
        if canonical in action.inputs.to_list()
        for output in action.outputs.to_list()
        if output.basename == canonical.basename
    ]
    asserts.equals(env, 1, len(aliases), "the compiler creates one dependency alias from the exact canonical File")
    if aliases:
        alias = aliases[0]
        data = ctx.file.data_copy
        runfiles = target[DefaultInfo].default_runfiles
        paths = [rlocation_path(ctx, file) for file in [alias, canonical, data]]
        visible = runfiles_scope_paths(ctx, runfiles, paths)
        asserts.false(env, alias.is_symlink, "the alias is a regular declared File whose transport need not retain a symlink")
        asserts.equals(env, canonical, visible.get(paths[0]), "ordinary aliases retain their canonical File without an ES twin or pool replacement")
        asserts.true(env, alias not in runfiles.files.to_list(), "ordinary Files cannot override the canonical alias mapping")
        asserts.equals(env, canonical, visible.get(paths[1]), "the canonical module retains its own coordinate")
        asserts.true(env, data != canonical and not data.is_symlink, "equal bytes belong to a distinct regular File")
        asserts.equals(env, data, visible.get(paths[2]), "unrelated equal-byte data retains its own File identity")
        launchers = [action for action in actions if any([file.basename == runnable_action_label(env).name + "_test_launcher.json" for file in action.outputs.to_list()])]
        asserts.equals(env, 1, len(launchers))
        if launchers:
            modules = json.decode(launchers[0].content)["runtime_modules"]
            asserts.true(env, paths[1] in modules, "the canonical module is materialized")
            asserts.true(env, paths[0] not in modules and paths[2] not in modules, "aliases and unrelated data cannot create another canonical module")
    return analysistest.end(env)

vitest_canonical_alias_test = analysistest.make(
    _vitest_canonical_alias_impl,
    attrs = {
        "canonical": attr.label(providers = [TsInfo]),
        "data_copy": attr.label(allow_single_file = True),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _config_json_inputs_impl(ctx):
    prefix = ctx.label.name
    program = ctx.actions.declare_file(prefix + "/program/check.test.js")
    config = ctx.actions.declare_file(prefix + "/config/vitest.config.mjs")
    scope = ctx.actions.declare_file(prefix + "/config/package.json")
    path = ctx.file.shadow_json.short_path[len(ctx.label.package) + 1:] if ctx.file.shadow_json else prefix + "/config/value.json"
    module = ctx.actions.declare_file(path)
    ctx.actions.write(program, "export {};\n")
    ctx.actions.write(config, "export default {};\n")
    ctx.actions.write(scope, '{"type":"module"}\n')
    ctx.actions.write(module, '{"runtime":true}\n')
    return [
        DefaultInfo(files = depset([program, module])),
        OutputGroupInfo(config = depset([config]), scope = depset([scope]), json = depset([module])),
    ]

_config_json_inputs = rule(
    implementation = _config_json_inputs_impl,
    attrs = {"shadow_json": attr.label(allow_single_file = True)},
)

_config_json_replacement_test = _fails_with("instead of published runtime File", "Did you mean")

def config_json_staging_case(name, shadow_json = None):
    _config_json_inputs(name = name + "_inputs", shadow_json = shadow_json)
    for output in ["config", "scope", "json"]:
        native.filegroup(name = name + "_" + output, srcs = [":" + name + "_inputs"], output_group = output)
    ts_test(
        name = name + "_subject",
        srcs = [":" + name + "_inputs"],
        config = ":" + name + "_config",
        config_srcs = [shadow_json] if shadow_json else [":" + name + "_scope", ":" + name + "_json"],
        node_modules = "//tests/npm:node_modules",
        # These synthetic config/module layouts are exercised by analysis assertions.
        tags = ["manual"],
        deps = ["@npm//:vitest"],
    )
    if shadow_json:
        _config_json_replacement_test(name = name + "_test", target_under_test = ":" + name + "_subject")
    else:
        runtime_scope_accepted_test(
            name = name + "_test",
            target_under_test = ":" + name + "_subject",
            json_module = ":" + name + "_json",
            staged_scope = ":" + name + "_scope",
        )

"""Analysis-time coverage for ts_compile.

Three kinds of assertion live here, none of which a build test can make:

  - what the rule writes at analysis and tells oxc -- the baseline tsconfig and
    the oxc command line, read straight out of the registered actions (the
    action tsconfig itself is tsaction's output; written_tsconfig_test.go reads
    it after the build);
  - that every guard fails, with the message that names the way out.

A guard's target is tagged manual so that `bazel build //...` does not try to
analyse it and stop on the very failure being asserted.
"""

load("@bazel_skylib//lib:paths.bzl", "paths")
load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//tests:runnable_actions.bzl", "runnable_action_aspect", "runnable_actions")
load("//tools/launcher:launcher.bzl", "rlocation_path", "runfiles_scope_paths")
load("//ts:defs.bzl", "TsInfo")
load("//ts/private:providers.bzl", "NodeModulesInfo", "require_emitted", "ts_info")

def _generated_type_inputs_impl(ctx):
    declaration = ctx.actions.declare_file("transitioned_types/value.d.ts")
    scope = ctx.actions.declare_file("transitioned_types/package.json")
    ctx.actions.write(declaration, "export type ProducerValue = 'declared-producer';\n")
    ctx.actions.write(scope, json.encode({
        "type": "module",
        "imports": {"#producer-value": "./value.d.ts"},
    }))
    return [DefaultInfo(files = depset([declaration, scope]))]

generated_type_inputs = rule(implementation = _generated_type_inputs_impl)

def _exec_type_inputs_impl(ctx):
    return [DefaultInfo(files = ctx.attr.producer[DefaultInfo].files)]

exec_type_inputs = rule(
    implementation = _exec_type_inputs_impl,
    attrs = {
        "producer": attr.label(cfg = "exec", mandatory = True),
    },
)

def _borrowed_importer_inputs_impl(ctx):
    env = analysistest.begin(ctx)
    source = ctx.attr.importer[NodeModulesInfo]
    info = analysistest.target_under_test(env)[TsInfo]
    companion = source.links["@types/culori"]
    consumer_companion = source.parent.links["@types/culori"]
    admitted = [source.links["culori"].link, companion.link, consumer_companion.link]
    unrelated = source.links["vitest"].link
    asserts.true(env, companion.store.tree != consumer_companion.store.tree, "the fixture must distinguish companion store Files")
    asserts.true(env, all([file in info.npm_files.to_list() for file in admitted]), "each origin retains its package and companion link Files")
    asserts.false(env, unrelated in info.npm_files.to_list(), "another package in the importer does not become an input")
    asserts.true(env, any([source.dir in record.importers for record in info.owners.to_list()]), "source consumers inherit the borrowed lookup directory")
    checks = [action for action in runnable_actions(env) if action.mnemonic == "TsgoCheck"]
    asserts.equals(env, 1, len(checks))
    if checks:
        generated = [file for file in info.sources.to_list() if not file.is_source]
        asserts.equals(env, 1, len(generated))
        asserts.true(env, all([file in checks[0].inputs.to_list() for file in generated]), "generated scalar inputs retain the same importer projection")
        asserts.true(env, all([file in checks[0].inputs.to_list() for file in admitted]), "both companion identities remain compiler action inputs")
        asserts.false(env, unrelated in checks[0].inputs.to_list(), "unrelated importer links stay outside the action")
        asserts.true(env, "-inherit_node_modules=" + source.dir in checks[0].argv, "the action mounts the original importer directory")
    ownership = _written_file_action(env, ".ownership")
    asserts.true(env, ownership != None, "the compiler retains its direct-store ownership manifest")
    if ownership:
        for selected in [companion, consumer_companion]:
            asserts.true(env, "npm-direct\t@types/culori\t" + selected.store.tree.path in ownership.content, "companion admission must not overwrite another origin's declaration")
    return analysistest.end(env)

borrowed_importer_inputs_test = analysistest.make(
    _borrowed_importer_inputs_impl,
    attrs = {"importer": attr.label(providers = [NodeModulesInfo], mandatory = True)},
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _written_file_action(env, suffix):
    for action in runnable_actions(env):
        outputs = action.outputs.to_list()
        if len(outputs) == 1 and outputs[0].basename.endswith(suffix):
            return action
    return None

def _runtime_npm_context_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = ctx.attr.producer[TsInfo]
    source = ctx.file.source
    runtime = {original: published for owner in info.owners.to_list() for original, published in getattr(owner, "runtime_files", ())}[source]
    selected = dict(info.transitive_es_twins.to_list())[runtime] if ctx.attr.twin else runtime
    coordinate = rlocation_path(ctx, runtime)
    importer = ctx.attr.importer[NodeModulesInfo]
    source_store = importer.links["@types/culori"].store.tree
    consumer_store = importer.parent.links["@types/culori"].store.tree
    asserts.true(env, source_store != consumer_store, "source and consumer stores must be distinct Files")
    asserts.equals(env, ctx.attr.twin, runtime != selected, "only the ES-twin case replaces the canonical File")
    if ctx.attr.source_mode:
        asserts.equals(env, source, runtime, "the source-mode fixture executes the original TypeScript File")
    config_action = _written_file_action(env, "_launcher.json")
    asserts.true(env, config_action != None, "the actual runtime consumer writes a launcher config")
    if config_action:
        config = json.decode(config_action.content)
        if config["mode"] in ["node", "node_test"]:
            spec_action = _written_file_action(env, "_launcher.runtime.json")
            builders = [action for action in runnable_actions(env) if action.mnemonic == "TsNativeView"]
            asserts.true(env, spec_action != None, "native runtime context is written to the build-owned view spec")
            asserts.equals(env, 1, len(builders), "one native view action consumes the runtime context")
            if spec_action == None or len(builders) != 1:
                return analysistest.end(env)
            asserts.true(env, spec_action.outputs.to_list()[0] in builders[0].inputs.to_list(), "the native view consumes the inspected context spec")
            spec = json.decode(spec_action.content)
            contexts = spec["npm_contexts"]
            modules = spec["modules"]
        else:
            contexts = config[config["mode"]].get("npm_contexts", [])
            modules = config["runtime_modules"]
        contexts = [context for context in contexts if context["source"] == source.short_path and context["module"] == coordinate]
        asserts.equals(env, 1, len(contexts), "the admitted source retains one context at its execution coordinate")
        if contexts:
            asserts.equals(env, rlocation_path(ctx, source_store), contexts[0]["bindings"].get("@types/culori"), "the context retains the source's exact store, not the consumer's version")
        asserts.true(env, coordinate in modules, "npm context is anchored to an admitted runtime module")
    runfiles = target[DefaultInfo].default_runfiles
    visible = runfiles_scope_paths(ctx, runfiles, [coordinate])
    asserts.equals(env, selected, visible.get(coordinate), "the context coordinate executes the selected exact File")
    asserts.true(env, source_store in runfiles.files.to_list(), "the selected source store is present in actual runfiles")
    return analysistest.end(env)

runtime_npm_context_test = analysistest.make(
    _runtime_npm_context_impl,
    attrs = {
        "importer": attr.label(providers = [NodeModulesInfo], mandatory = True),
        "producer": attr.label(providers = [TsInfo], mandatory = True),
        "source": attr.label(allow_single_file = True, mandatory = True),
        "source_mode": attr.bool(),
        "twin": attr.bool(),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _emit_command_line_impl(ctx):
    env = analysistest.begin(ctx)
    emit_actions = [
        action
        for action in runnable_actions(env)
        if action.mnemonic == "TsEmit"
    ]

    # One invocation, not one per directory: the root is the package, so a
    # source's depth below it survives into the output directory.
    asserts.equals(env, 1, len(emit_actions), "TsEmit actions")
    if len(emit_actions) != 1:
        return analysistest.end(env)

    info = analysistest.target_under_test(env)[TsInfo]
    published = depset(transitive = [info.js, info.js_maps, info.declarations]).to_list()
    asserts.true(env, all([file in published for file in emit_actions[0].outputs.to_list()]), "emission produces only published code, maps and declarations")
    argv = emit_actions[0].argv
    out_dirs = [a for a in argv if a.startswith("-out_dir=")]
    asserts.equals(env, 1, len(out_dirs), "-out_dir flags")
    asserts.true(
        env,
        out_dirs[0].endswith("/tests/compiler_options/analysis"),
        "-out_dir is the package's bin directory: " + out_dirs[0],
    )
    asserts.equals(
        env,
        ["-root=tests/compiler_options/analysis"],
        [a for a in argv if a.startswith("-root=")],
        "-root",
    )
    return analysistest.end(env)

emit_command_line_test = analysistest.make(_emit_command_line_impl, extra_target_under_test_aspects = [runnable_action_aspect])

def _declare_command_line_impl(ctx):
    env = analysistest.begin(ctx)
    declares = [
        action
        for action in runnable_actions(env)
        if action.mnemonic == "TsgoDeclare"
    ]
    asserts.equals(env, 1, len(declares), "TsgoDeclare actions")
    if len(declares) != 1:
        return analysistest.end(env)

    argv = declares[0].argv
    asserts.false(env, any([arg.startswith("-oxc=") for arg in argv]), "declaration emission does not require an observer executable")
    asserts.true(env, "-declarations_only" in argv, "the declaration action uses the checked tsgo program")
    asserts.equals(env, ["-root=" + ctx.attr.root_dir], [arg for arg in argv if arg.startswith("-root=")], "physical source root")
    out_dirs = [arg for arg in argv if arg.startswith("-out_dir=")]
    asserts.equals(env, 1, len(out_dirs), "one declaration output directory")
    if out_dirs:
        asserts.true(env, out_dirs[0].endswith("/tests/compiler_options/analysis"), "declarations remain in the target's output namespace")
    return analysistest.end(env)

declare_command_line_test = analysistest.make(
    _declare_command_line_impl,
    attrs = {
        "root_dir": attr.string(
            mandatory = True,
            doc = "The physical source root passed to the declaration emitter.",
        ),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

# Restated, not imported: a change to _BASELINE_OPTIONS has to be made here too.
_BASELINE_KEYS = {
    "strict": True,
    "module": "Preserve",
    "target": "es2022",
    "jsx": "react-jsx",
    "skipLibCheck": True,
    "esModuleInterop": True,
}

def _baseline_file_impl(ctx):
    """The baseline is a file the action config extends FIRST, and it names no resolver.

    TypeScript couples moduleResolution to `module`: a layer that owns the one
    may state the other, and the baseline's `module` is beaten by any tsconfig
    that sets its own, so it leaves the resolver to tsgo to derive.
    """
    env = analysistest.begin(ctx)
    baseline = _written_file_action(env, ".tsconfig_baseline.json")
    asserts.true(env, baseline != None, "ts_compile wrote no baseline to extend")
    if baseline == None:
        return analysistest.end(env)

    opts = json.decode(baseline.content)["compilerOptions"]
    for key, want in _BASELINE_KEYS.items():
        asserts.equals(env, want, opts.get(key), key)
    asserts.equals(env, None, opts.get("moduleResolution"), "moduleResolution stays out of the baseline")
    asserts.equals(env, sorted(_BASELINE_KEYS), sorted(opts), "the baseline holds these keys and no other")
    return analysistest.end(env)

baseline_file_test = analysistest.make(_baseline_file_impl, extra_target_under_test_aspects = [runnable_action_aspect])

def _fails_with(message, config_settings = {}):
    def _impl(ctx):
        env = analysistest.begin(ctx)
        for expected in message if type(message) == "list" else [message]:
            asserts.expect_failure(env, expected)
        return analysistest.end(env)

    return analysistest.make(_impl, expect_failure = True, config_settings = config_settings)

def _scope_alias_member_impl(ctx):
    info = ctx.attr.producer[TsInfo]
    alias = [runtime for owner in info.owners.to_list() for source, runtime in getattr(owner, "runtime_scopes", ()) if source == ctx.file.scope][0]
    fields = {field: getattr(info, field) for field in dir(info) if field not in ["to_json", "to_proto"]}
    fields["owners"] = depset([struct(
        label = str(ctx.label),
        files = depset([alias] + ([ctx.file.other] if ctx.file.other else [])),
        declarations = depset(),
        type_inputs = depset(),
        importers = (),
        runtime_files = ((ctx.file.scope, alias),) if ctx.attr.json_module else (),
        canonical_links = ((alias, ctx.file.other),) if ctx.file.other else (),
    )], transitive = [info.owners])
    return [TsInfo(**fields), DefaultInfo(files = ctx.attr.producer[DefaultInfo].files)]

scope_alias_member = rule(
    implementation = _scope_alias_member_impl,
    attrs = {
        "producer": attr.label(providers = [TsInfo], mandatory = True),
        "scope": attr.label(allow_single_file = True, mandatory = True),
        "other": attr.label(allow_single_file = True),
        "json_module": attr.bool(),
    },
)

def _member_scope_alias_impl(ctx):
    env = analysistest.begin(ctx)
    aliases = [runtime for owner in ctx.attr.producer[TsInfo].owners.to_list() for source, runtime in getattr(owner, "runtime_scopes", ()) if source == ctx.file.scope]
    actions = [action for action in analysistest.target_actions(env) if action.mnemonic == "NpmStore"]
    asserts.equals(env, 1, len(aliases))
    asserts.equals(env, 1, len(actions))
    if aliases and actions:
        alias = aliases[0]
        asserts.true(env, alias != ctx.file.scope, "the member copies the producer's no-transform scope projection")
        asserts.true(env, alias in actions[0].inputs.to_list(), "metadata-only alias remains an input to the npm copy action")
    return analysistest.end(env)

member_scope_alias_test = analysistest.make(
    _member_scope_alias_impl,
    attrs = {
        "producer": attr.label(providers = [TsInfo]),
        "scope": attr.label(allow_single_file = True),
    },
)

member_scope_module_alias_test = _fails_with(["requires canonical dependency", "the npm store copies files"])
member_scope_alias_conflict_test = _fails_with(["has conflicting canonical Files", "Did you mean"])

declaration_map_without_tsgo_test = _fails_with(
    "--//ts:declaration_map needs the tsgo declaration emit",
    config_settings = {
        str(Label("//ts:declarations")): "oxc",
        str(Label("//ts:declaration_map")): True,
    },
)
non_scope_metadata_test = _fails_with("is not a package.json File")
scope_destination_conflict_test = _fails_with([
    "runtime package scope 'tests/compiler_options/analysis/scope_asset/package.json'",
    "already occupied by 'tests/compiler_options/analysis/scope_asset/package.json' from",
    ":conflicting_scope_asset",
])
opaque_scope_destination_test = _fails_with([
    "runtime package scope 'tests/compiler_options/analysis/scope_asset/package.json'",
    "already occupied by opaque runtime File",
    ":conflicting_scope_asset",
])

def _mixed_source_roots_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[TsInfo]
    prefix = "tests/compiler_options/analysis/"
    expected = [(prefix + "generated/gen.ts", prefix + "generated/gen.d.ts"), (prefix + "lone.ts", prefix + "lone.d.ts")]
    pairs = [pair for owner in info.owners.to_list() for pair in owner.declaration_files]
    asserts.equals(env, expected, sorted([(source.short_path, output.short_path) for source, output in pairs]), "generated and authored declarations retain exact source origins")
    asserts.equals(env, [False, True], [source.is_source for source, _output in sorted(pairs, key = _source_short_path)], "the fixture spans generated and authored physical roots")
    asserts.equals(env, sorted([output for _source, output in pairs], key = _short_path), sorted(info.declarations.to_list(), key = _short_path), "the provider publishes those exact declaration Files")
    declares = [action for action in runnable_actions(env) if action.mnemonic == "TsgoDeclare"]
    asserts.equals(env, 1, len(declares), "one checked declaration action spans both physical roots")
    if declares:
        action = declares[0]
        asserts.equals(env, sorted([output for _source, output in pairs], key = _short_path), sorted(action.outputs.to_list(), key = _short_path), "the checked compiler writes the published Files")
        asserts.true(env, "emit" in action.argv and "-declarations_only" in action.argv, "default checked declaration emission")
        asserts.equals(env, 1, len([arg for arg in action.argv if arg.startswith("-tsgo=")]), "one tsgo compiler")
        asserts.equals(env, [], [arg for arg in action.argv if arg.startswith("-oxc=") or arg == "-declarations"], "declarations do not use the isolated backend")
        asserts.equals(env, sorted(["-root=" + source.dirname[:-len("/generated")] if source.basename == "gen.ts" else "-root=" + source.dirname for source, _output in pairs]), sorted([arg for arg in action.argv if arg.startswith("-root=")]), "each physical root preserves the source's logical package-relative path")
        for source, _output in pairs:
            asserts.true(env, source in action.inputs.to_list() and source.path in action.argv, "the declaration action reads its exact source: " + source.short_path)
    return analysistest.end(env)

def _short_path(file):
    return file.short_path

def _source_short_path(pair):
    return pair[0].short_path

mixed_source_roots_test = analysistest.make(_mixed_source_roots_impl, extra_target_under_test_aspects = [runnable_action_aspect])
jsx_source_test = _fails_with("every jsx mode but preserve")

def _declaration_content_edges_impl(ctx):
    env = analysistest.begin(ctx)
    seen = {}
    for action in runnable_actions(env):
        if action.mnemonic not in ["TsConfig", "TsEmit", "TsgoCheck"]:
            continue
        seen[action.mnemonic] = True
        if action.mnemonic == "TsEmit":
            if ctx.attr.full_emit and "-es_modules" not in action.argv:
                asserts.false(env, any([arg.startswith("-oxc=") for arg in action.argv]), "tsgo emission does not require an observer executable")
        present = any([file.path.endswith(ctx.attr.declaration) for file in action.inputs.to_list()])
        expected = action.mnemonic == "TsgoCheck" or (ctx.attr.full_emit and action.mnemonic == "TsEmit" and "-es_modules" not in action.argv) or (ctx.attr.tree and action.mnemonic == "TsConfig")
        asserts.equals(env, expected, present, action.mnemonic + " declaration content dependency")
    asserts.equals(env, ["TsConfig", "TsEmit", "TsgoCheck"], sorted(seen.keys()))
    return analysistest.end(env)

declaration_content_edges_test = analysistest.make(
    _declaration_content_edges_impl,
    attrs = {
        "declaration": attr.string(mandatory = True),
        "full_emit": attr.bool(),
        "tree": attr.bool(),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _json_module_survives_metadata_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[TsInfo]
    source = ctx.file.source
    mappings = [pair for owner in info.owners.to_list() for pair in owner.runtime_files if pair[0] == source]
    asserts.equals(env, 1, len(mappings), "metadata cannot erase the JSON module's source association")
    if mappings:
        runtime = mappings[0][1]
        asserts.equals(env, "tests/compiler_options/analysis/layout/authored.json", runtime.short_path, "the JSON module follows the common emitted layout")
        asserts.false(env, runtime.is_source, "the foreign JSON module is projected into the output namespace")
        asserts.true(env, runtime in info.transitive_data.to_list(), "the exact published JSON File remains a runtime input")
    return analysistest.end(env)

json_module_survives_metadata_test = analysistest.make(
    _json_module_survives_metadata_impl,
    attrs = {"source": attr.label(allow_single_file = True, mandatory = True)},
)

def _independent_library_roots_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[TsInfo]
    prefix = "tests/compiler_options/analysis/"
    outputs = ["analysis/mixed_generated_layout", "layout/value"]
    asserts.equals(env, [prefix + name + ".js" for name in outputs], sorted([file.short_path for file in info.js.to_list()]))
    asserts.equals(env, [prefix + name + ".d.ts" for name in outputs], sorted([file.short_path for file in info.declarations.to_list()]))
    return analysistest.end(env)

independent_library_roots_test = analysistest.make(_independent_library_roots_impl)

def _generated_runtime_layout_impl(ctx):
    env = analysistest.begin(ctx)
    actions = runnable_actions(env)
    asserts.true(env, any([action.mnemonic == "TsEmit" for action in actions]), "local TypeScript still emits beside generated runtime inputs")
    return analysistest.end(env)

generated_runtime_layout_test = analysistest.make(
    _generated_runtime_layout_impl,
    config_settings = {str(Label("//ts:declarations")): "oxc"},
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _declaration_ownership_runtime_alias_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[TsInfo]
    source = ctx.file.source
    owner = [record for record in info.owners.to_list() if record.label == str(target.label).removeprefix("@@")][0]
    declarations = [output for original, output in owner.declaration_files if original == source]
    prefix = "tests/compiler_options/analysis/analysis/generated_cycle/"
    asserts.equals(env, [prefix + "entry.d.mts"], [file.short_path for file in declarations], "the relocated consumer owns the generated module's checked declaration")
    producer = ctx.attr.producer[TsInfo]
    modules = producer.js.to_list()
    asserts.equals(env, ["back.mjs", "entry.mjs"], sorted([file.basename for file in modules]), "both generated cycle modules are published")
    asserts.true(env, source in modules, "the compiler source is the producer's exact canonical File")
    runtime_pairs = [pair for record in info.owners.to_list() for pair in getattr(record, "runtime_files", ()) if pair[0] in modules]
    asserts.equals(env, sorted([(file.path, file.path) for file in modules]), sorted([(original.path, runtime.path) for original, runtime in runtime_pairs]), "declaration emission cannot create another runtime owner for the cycle")
    for module in modules:
        asserts.false(env, module.is_source, "the cycle must use generated Files")
        asserts.true(env, module in info.transitive_js.to_list(), "the producer's canonical module remains live")
        aliases = [link for link, canonical in owner.canonical_links if canonical == module]
        asserts.equals(env, [prefix + module.basename], [link.short_path for link in aliases], "an owned declaration must not remove the required runtime alias for " + module.basename)
        for alias in aliases:
            asserts.true(env, alias in info.transitive_data.to_list(), "the native consumer receives the canonical runtime alias")
    return analysistest.end(env)

declaration_ownership_runtime_alias_test = analysistest.make(
    _declaration_ownership_runtime_alias_impl,
    attrs = {
        "producer": attr.label(providers = [TsInfo], mandatory = True),
        "source": attr.label(allow_single_file = True, mandatory = True),
    },
    config_settings = {str(Label("//ts:declarations")): "tsgo"},
)

def _dev_server_generated_dependency_files_impl(ctx):
    env = analysistest.begin(ctx)
    runfiles = analysistest.target_under_test(env)[DefaultInfo].default_runfiles.files.to_list()
    entry = ctx.attr.entry[TsInfo]
    producer = ctx.attr.producer[TsInfo]
    asserts.equals(env, ["package.json", "value.js", "value.json"], sorted([source.basename for source in ctx.files.originals]), "the fixture includes generated JavaScript, JSON and package scope")
    for source in ctx.files.originals:
        asserts.false(env, source.is_source, "the resolver must retain the exact generated input")
        asserts.false(env, source in entry.sources.to_list(), "the input belongs to a transitive producer, not the entry's source set")
        field = "runtime_scopes" if source.basename == "package.json" else "runtime_files"
        pairs = [pair for owner in producer.owners.to_list() for pair in getattr(owner, field, ()) if pair[0] == source]
        destination = "tests/compiler_options/analysis/analysis/dev_generated/" + source.basename
        moved = [runtime for _original, runtime in pairs if runtime.short_path == destination]
        asserts.equals(env, 1, len(moved), "one producer association at the moved destination for " + source.basename)
        if source.basename == "package.json":
            asserts.true(env, (source, source) in pairs, "the original generated scope keeps its exact published identity")
        if moved:
            runtime = moved[0]
            asserts.true(env, source != runtime, "generated source and moved runtime have distinct File identities")
            asserts.equals(env, destination, runtime.short_path, "the fixture moves the producer's logical source coordinates")
            live = entry.transitive_js.to_list() if source.extension == "js" else entry.transitive_data.to_list()
            asserts.true(env, runtime in live, "the canonical runtime File is live in the entry's transitive closure")
            inherited = [pair for owner in entry.owners.to_list() for pair in getattr(owner, field, ())]
            asserts.true(env, (source, runtime) in inherited, "the entry retains the producer's exact source/runtime association")
            asserts.true(env, source in runfiles, "the dev resolver can read the exact generated original: " + source.short_path)
            asserts.true(env, runtime in runfiles, "the dev server also retains the canonical moved File: " + runtime.short_path)
            if source.basename != "package.json":
                asserts.false(env, runtime in entry.transitive_runtime_sources.to_list(), "the live JavaScript or JSON module is outside the TypeScript runtime-source set")
        if source.basename != "package.json":
            asserts.false(env, source in entry.transitive_runtime_sources.to_list(), "a TypeScript runtime-source filter cannot recover generated JS or JSON originals")
    asset = ctx.file.asset
    asset_coordinate = "tests/compiler_options/analysis/tests/compiler_options/layout/authored.json"
    assets = [(original, coordinate, published) for owner in entry.owners.to_list() for original, coordinate, published in getattr(owner, "asset_files", ()) if original == asset]
    asserts.true(env, bool(assets), "the moved source consumer retains its foreign packaged asset authority")
    metadata = ctx.file.metadata
    compiler_inputs = depset(transitive = [owner.type_inputs for owner in entry.owners.to_list()]).to_list()
    asserts.true(env, asset in compiler_inputs, "explicit data also used as type_inputs keeps its exact compiler File")
    asserts.true(env, metadata in compiler_inputs, "metadata-only input keeps its exact compiler File")
    asserts.false(env, any([original == metadata for owner in entry.owners.to_list() for original, _coordinate, _published in getattr(owner, "asset_files", ())]), "metadata-only inputs never acquire an asset coordinate")
    asserts.false(env, metadata in entry.transitive_data.to_list(), "metadata-only inputs never enter runtime data")
    modules = [pair for owner in entry.owners.to_list() for pair in getattr(owner, "runtime_files", ())]
    asserts.false(env, any([source == asset or runtime == asset for source, runtime in modules]), "a standalone JSON asset never becomes a module")
    asserts.true(env, any([source in entry.sources.to_list() and runtime in entry.runtime_sources.to_list() and source != runtime for source, runtime in modules]), "the source-mode caller actually moves to follow its emitted dependency")
    for original, coordinate, published in assets:
        asserts.equals(env, asset_coordinate, coordinate, "foreign data keeps its consumer-package coordinate rather than its source path")
        asserts.true(env, published in entry.transitive_data.to_list(), "only a live asset supplies the source view")
        asserts.true(env, published.short_path != coordinate, "the fixture moves the runtime asset away from the dev lookup")
        asserts.true(env, published in runfiles, "the canonical runtime asset remains published")
    unmoved = ctx.attr.unmoved[TsInfo]
    asserts.equals(env, unmoved.sources.to_list(), unmoved.runtime_sources.to_list(), "foreign asset coordinates alone must not move source modules")
    unmoved_assets = [(original, coordinate, published) for owner in unmoved.owners.to_list() for original, coordinate, published in getattr(owner, "asset_files", ()) if original == asset]
    asserts.equals(env, 1, len(unmoved_assets), "the unmoved control publishes its ordinary asset")
    for _original, coordinate, published in unmoved_assets:
        asserts.equals(env, asset_coordinate, coordinate)
        asserts.equals(env, coordinate, published.short_path, "identity layout keeps the package-local asset path")
    extended = ctx.attr.extended[TsInfo]
    extended_sources = extended.sources.to_list()
    extended_modules = [runtime for owner in extended.owners.to_list() for source, runtime in getattr(owner, "runtime_files", ()) if source in extended_sources and runtime in extended.runtime_sources.to_list()]
    asserts.equals(env, 1, len(extended_modules), "one moved source-mode entry follows the relocated dependency")
    outside = ctx.file.outside_asset
    coordinate = "tests/workers_nested/src/greeting.txt"
    destination = "tests/compiler_options/analysis/workers_nested/src/greeting.txt"
    outside_bindings = [(original, logical, published) for owner in extended.owners.to_list() for original, logical, published in getattr(owner, "asset_files", ()) if published.short_path == destination]
    asserts.equals(env, 1, len(outside_bindings), "an asset outside the prior module root extends the moved consumer's shared layout")
    if extended_modules and outside_bindings:
        module = extended_modules[0]
        original, outside_coordinate, published = outside_bindings[0]
        asserts.equals(env, "tests/compiler_options/analysis/compiler_options/analysis/lone.ts", module.short_path, "the module follows the same root extended for the asset")
        asserts.equals(env, outside, original, "the consumer retains the exact foreign asset authority")
        asserts.equals(env, coordinate, outside_coordinate)
        asserts.equals(env, published.path, paths.normalize(paths.join(module.dirname, "../../workers_nested/src/greeting.txt")), "the retained source-relative asset edge reaches the published File")
        asserts.true(env, published in extended.transitive_data.to_list(), "the destination is an actual runtime asset")
    config = _written_file_action(env, "vite.config.mjs")
    asserts.true(env, config != None, "the actual dev server generates its plugin configuration")
    if config:
        bindings = ["[{}]: {{ path: path.resolve(fs.realpathSync(bazelBin), {}), context: \"asset\", isSource: false }}".format(json.encode(asset_coordinate), json.encode(published.short_path)) for _original, _coordinate, published in assets]
        asserts.true(env, any([binding in config.content for binding in bindings]), "the asset coordinate resolves to a declared published File")
        for source in ctx.files.originals + entry.sources.to_list():
            if source.basename == "package.json":
                continue
            base = "workspaceRoot" if source.is_source else "fs.realpathSync(bazelBin)"

            # A package scope may follow the binding.
            binding = "[{}]: {{ path: path.resolve({}, {}), context: \"source\", isSource: {}".format(json.encode(source.short_path), base, json.encode(source.short_path), json.encode(source.is_source))
            asserts.true(env, binding in config.content, "emitted and source-mode modules select their original File: " + source.short_path)
        asserts.false(env, "[{}]:".format(json.encode(metadata.short_path)) in config.content, "compiler-only metadata never becomes a dev module")
    return analysistest.end(env)

dev_server_generated_dependency_files_test = analysistest.make(
    _dev_server_generated_dependency_files_impl,
    attrs = {
        "asset": attr.label(allow_single_file = True, mandatory = True),
        "metadata": attr.label(allow_single_file = True, mandatory = True),
        "extended": attr.label(providers = [TsInfo], mandatory = True),
        "outside_asset": attr.label(allow_single_file = True, mandatory = True),
        "unmoved": attr.label(providers = [TsInfo], mandatory = True),
        "entry": attr.label(providers = [TsInfo], mandatory = True),
        "producer": attr.label(providers = [TsInfo], mandatory = True),
        "originals": attr.label_list(allow_files = True, mandatory = True),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _previous_shape_provider_impl(ctx):
    declaration = ctx.actions.declare_file(ctx.label.name + ".d.ts")
    ctx.actions.write(declaration, "export declare const previous: number;\n")
    runtime_files = []
    if ctx.attr.runtime:
        runtime = ctx.actions.declare_file(ctx.label.name + ".js")
        if ctx.file.runtime_src:
            ctx.actions.symlink(output = runtime, target_file = ctx.file.runtime_src)
        else:
            ctx.actions.write(runtime, "export const previous = 42;\n")
        runtime_files.append(runtime)
    data_files = []
    if ctx.attr.data_path:
        data_file = ctx.actions.declare_file(ctx.attr.data_path)
        ctx.actions.write(data_file, '{"name":"different-scope"}\n')
        data_files.append(data_file)
    declarations = depset([declaration], order = "postorder")
    js = depset(runtime_files, order = "postorder")
    data = depset(data_files, order = "postorder")
    files = depset([declaration] + runtime_files + data_files, order = "postorder")
    empty = depset(order = "postorder")
    return [
        DefaultInfo(files = files),
        TsInfo(
            js = js,
            js_maps = empty,
            runtime_sources = empty,
            runtime_source_owners = empty,
            transitive_runtime_sources = empty,
            declarations = declarations,
            data = data,
            manifest = None,
            sources = declarations,
            tsconfig = None,
            transitive_js = js,
            transitive_js_maps = empty,
            transitive_data = data,
            transitive_es_twins = empty,
            npm_packages = empty,
            npm_files = empty,
            owners = depset([struct(
                label = str(ctx.label).removeprefix("@@"),
                files = depset([declaration] + data_files, order = "postorder"),
                declarations = declarations,
                type_inputs = declarations,
                importers = (),
            )], order = "postorder"),
        ),
    ]

previous_shape_provider = rule(
    implementation = _previous_shape_provider_impl,
    attrs = {
        "data_path": attr.string(),
        "runtime": attr.bool(),
        "runtime_src": attr.label(allow_single_file = [".js"]),
    },
)

def _scope_admission_impl(ctx):
    runtime = ctx.actions.declare_file(ctx.label.name + "/nested/value.js")
    ctx.actions.write(runtime, "export const value = 42;\n")
    scope = ctx.file.scope
    data = [scope]
    pairs = [(scope, scope)]
    if ctx.attr.shadowed:
        supplied = ctx.actions.declare_file(ctx.label.name + "/package.json")
        closer = ctx.actions.declare_file(ctx.label.name + "/nested/package.json")
        ctx.actions.symlink(output = supplied, target_file = scope)
        ctx.actions.write(closer, '{"type":"commonjs"}\n')
        data.extend([supplied, closer])
        pairs.append((scope, supplied))
    require_emitted(ctx.label, struct(
        transitive_runtime_sources = depset(),
        transitive_js = depset([runtime] if ctx.attr.live else []),
        transitive_data = depset(data),
        owners = depset([struct(
            runtime_files = ((ctx.file.src, runtime),),
            runtime_scopes = tuple(pairs),
        )]),
    ), "analysis consumer")
    return [DefaultInfo()]

scope_admission = rule(
    implementation = _scope_admission_impl,
    attrs = {
        "live": attr.bool(),
        "scope": attr.label(allow_single_file = True),
        "shadowed": attr.bool(),
        "src": attr.label(allow_single_file = True),
    },
)

missing_runtime_scope_test = _fails_with("requires runtime package scope")
shadowed_runtime_scope_test = _fails_with("is shadowed by runtime manifest")

def _republished_runtime_owner_impl(ctx):
    asset_source, asset = [
        pair
        for owner in ctx.attr.asset[TsInfo].owners.to_list()
        for pair in owner.runtime_files
        if pair[0].extension == "json"
    ][0]
    runtime = ctx.actions.declare_file("scope_asset/" + ctx.label.name + "/value.js")
    source_runtime = ctx.actions.declare_file("scope_asset/" + ctx.label.name + "/source.ts")
    json_runtime = ctx.actions.declare_file("scope_asset/" + ctx.label.name + "/config.json")
    scope = ctx.actions.declare_file("scope_asset/" + ctx.label.name + "/package.json")
    ctx.actions.write(runtime, "export const value = 42;\n")
    ctx.actions.symlink(output = source_runtime, target_file = ctx.file.src)
    ctx.actions.symlink(output = json_runtime, target_file = ctx.file.json)
    ctx.actions.symlink(output = scope, target_file = ctx.file.scope)
    info = ts_info()
    fields = {field: getattr(info, field) for field in dir(info) if field not in ["to_json", "to_proto"]}
    fields["owners"] = depset([struct(
        label = str(ctx.label).removeprefix("@@"),
        files = depset(),
        declarations = depset(),
        type_inputs = depset(),
        importers = (),
        runtime_files = ((ctx.file.src, runtime), (ctx.file.src, source_runtime), (ctx.file.json, json_runtime)),
        runtime_scopes = ((ctx.file.scope, scope),),
        asset_files = ((asset_source, asset_source.short_path, asset),),
    )])
    return [TsInfo(**fields), DefaultInfo(files = depset([runtime, source_runtime, json_runtime, scope, asset]))]

republished_runtime_owner = rule(
    implementation = _republished_runtime_owner_impl,
    attrs = {
        "asset": attr.label(providers = [TsInfo], mandatory = True),
        "json": attr.label(allow_single_file = True, mandatory = True),
        "scope": attr.label(allow_single_file = True),
        "src": attr.label(allow_single_file = True),
    },
)

def _data_publication_identity_impl(ctx):
    env = analysistest.begin(ctx)
    producer = ctx.attr.producer[TsInfo]
    for field in ["js", "transitive_js", "data", "transitive_data"]:
        asserts.equals(env, [], getattr(producer, field).to_list(), "the provider wire carries files only through DefaultInfo: " + field)
    owner = producer.owners.to_list()[0]
    requested = ctx.attr.producer[DefaultInfo].files.to_list()
    assets = [(source, file) for source, _coordinate, file in owner.asset_files]
    asset_modules = [pair for record in ctx.attr.asset_producer[TsInfo].owners.to_list() for pair in record.runtime_files if pair[0].extension == "json"]
    asserts.equals(env, 1, len(asset_modules), "one shared producer owns the fixture's JSON runtime File")
    asserts.equals(env, asset_modules, assets, "the standalone asset forwards the shared producer's exact source/runtime Files")
    asserts.equals(env, [source.short_path for source, _file in asset_modules], [coordinate for _source, coordinate, _file in owner.asset_files], "the asset retains its original coordinate across the shared producer")
    for _source, file in list(owner.runtime_files) + list(owner.runtime_scopes) + assets:
        asserts.true(env, file in requested, "DefaultInfo requests each runtime, scope and asset File")
    for consumer in [analysistest.target_under_test(env), ctx.attr.relocated]:
        info = consumer[TsInfo]
        owners = info.owners.to_list()
        relocated = consumer.label == ctx.attr.relocated.label
        asserts.equals(env, relocated, bool(info.transitive_js.to_list()), "the relocated variant retains its emitted dependency closure")
        asserts.equals(env, [owner], [record for record in owners if record.label == owner.label])
        live = {file: True for file in info.transitive_data.to_list() + info.transitive_js.to_list()}
        for _source, file in list(owner.runtime_files) + list(owner.runtime_scopes) + assets:
            asserts.true(env, file in live, "data must retain the producer's canonical File")
        for source, file in assets:
            publications = {published: True for record in owners for original, _coordinate, published in getattr(record, "asset_files", ()) if original == source}
            asserts.true(env, file in publications, "the consumer reuses the shared File at the asset's original coordinate")
            asserts.true(env, all([published in info.transitive_data.to_list() for published in publications]), "each asset placement is a live runtime data File")
            asserts.equals(env, 1 if relocated else 0, len([published for published in publications if published != file]), "only the relocated consumer creates a distinct package-local asset File")
        require_emitted(ctx.label, info, "analysis consumer")
    return analysistest.end(env)

data_publication_identity_test = analysistest.make(
    _data_publication_identity_impl,
    attrs = {
        "asset_producer": attr.label(providers = [TsInfo], mandatory = True),
        "producer": attr.label(providers = [TsInfo], mandatory = True),
        "relocated": attr.label(providers = [TsInfo], mandatory = True),
    },
)

def _previous_shape_provider_consumption_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[TsInfo]
    previous = ctx.attr.dep[TsInfo]
    original = previous.owners.to_list()[0]
    owners = info.owners.to_list()
    forwarded = [record for record in owners if record.label == original.label]
    asserts.equals(env, [original], forwarded, "the original opaque owner survives consumption")
    asserts.false(env, hasattr(original, "runtime_files"), "fixture uses the previous public owner shape")
    asserts.false(env, hasattr(original, "runtime_scopes"), "scope provenance is optional for previous owners")
    for runtime in previous.js.to_list():
        asserts.false(env, runtime in original.files.to_list(), "the previous owner shape omits emitted JavaScript from files")
        asserts.true(env, runtime in info.transitive_js.to_list(), "the producer's exact runtime File survives")
    data = info.transitive_data.to_list()
    for asset in previous.data.to_list():
        asserts.true(env, asset in data, "opaque runtime data survives without a new scope placement demand")
        if asset in data:
            for direct in info.data.to_list():
                asserts.true(env, asset == direct or data.index(asset) < data.index(direct), "dependency runtime data retains postorder before distinct direct data")
    published = previous.js.to_list() + previous.declarations.to_list() + previous.data.to_list()
    asserts.equals(env, [], [
        pair
        for record in owners
        if hasattr(record, "runtime_files")
        for pair in record.runtime_files
        if pair[0] in published or pair[1] in published
    ], "consumption does not invent source/runtime mappings for opaque artifacts")
    asserts.equals(env, [], [
        pair
        for record in owners
        if hasattr(record, "runtime_scopes")
        for pair in record.runtime_scopes
        if pair[0] in published or pair[1] in published
    ], "consumption does not invent scope provenance for opaque artifacts")
    actions = runnable_actions(env)
    asserts.true(env, any([action.mnemonic == "TsEmit" for action in actions]), "consumer still emits")
    checks = [action for action in actions if action.mnemonic == "TsgoCheck"]
    asserts.equals(env, 1, len(checks), "consumer still validates")
    if checks:
        for declaration in previous.declarations.to_list():
            asserts.true(env, declaration in checks[0].inputs.to_list(), "the exact declaration reaches the compiler")
    return analysistest.end(env)

previous_shape_provider_consumption_test = analysistest.make(
    _previous_shape_provider_consumption_impl,
    attrs = {"dep": attr.label(providers = [TsInfo], mandatory = True)},
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _source_publication_identity_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[TsInfo]
    owners = info.owners.to_list()
    modules = [pair for owner in owners for pair in owner.runtime_files]
    live = depset(transitive = [info.transitive_js, info.transitive_runtime_sources, info.transitive_data]).to_list()
    for source in ctx.files.originals:
        asserts.equals(env, [(source, source)], [pair for pair in modules if pair[0] == source], "unchanged source publication retains the producer's exact File")
        asserts.equals(env, [source], [file for file in live if file.short_path == source.short_path], "one original File occupies each unchanged module coordinate")
    scope = ctx.file.scope
    scopes = [pair for owner in owners for pair in getattr(owner, "runtime_scopes", ()) if pair[0] == scope]
    asserts.equals(env, [(scope, scope)], scopes, "generated source storage cannot create a different runtime package scope")
    asserts.equals(env, [scope], [file for file in info.transitive_data.to_list() if file.short_path == scope.short_path], "config staging and runtime publication select the same authored scope")
    asserts.true(env, any([scope in owner.type_inputs.to_list() for owner in owners]), "runtime publication preserves original compiler metadata")
    asserts.false(env, scope in info.sources.to_list(), "metadata never becomes a module root")
    checks = [action for action in runnable_actions(env) if action.mnemonic == "TsgoCheck"]
    asserts.equals(env, 1, len(checks), "the source program still validates")
    if checks:
        asserts.true(env, scope in checks[0].inputs.to_list(), "validation reads the original scope")
    assets = [published for owner in owners for original, _coordinate, published in getattr(owner, "asset_files", ()) if original == ctx.file.asset]
    asserts.equals(env, 1, len(assets), "explicit data retains its asset publication")
    if assets:
        asserts.true(env, assets[0] != ctx.file.asset and assets[0] in info.transitive_data.to_list(), "standalone data remains staged independently of JSON module publication")
    return analysistest.end(env)

source_publication_identity_test = analysistest.make(
    _source_publication_identity_impl,
    attrs = {
        "originals": attr.label_list(allow_files = True, mandatory = True),
        "scope": attr.label(allow_single_file = True, mandatory = True),
        "asset": attr.label(allow_single_file = True, mandatory = True),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _scope_inputs_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[TsInfo]
    owners = info.owners.to_list()
    inputs = depset(transitive = [owner.type_inputs for owner in owners]).to_list()
    scopes = [file for file in inputs if file.short_path == ctx.attr.scope]
    asserts.equals(env, 1, len(scopes), "compiler metadata retains one original scope File")
    if len(scopes) != 1:
        return analysistest.end(env)
    scope = scopes[0]
    asserts.equals(env, ctx.attr.scope_source, scope.is_source, "compiler metadata retains source/generated identity")
    if not scope.is_source:
        asserts.equals(env, "scope_metadata", scope.owner.name, "compiler metadata retains the original scope producer")
    runtime = [file for file in info.transitive_data.to_list() if file.short_path == scope.short_path]
    asserts.equals(env, 0 if ctx.attr.compiler_only else 1, len(runtime), "only runtime scope demand supplies a runtime entry")
    if len(runtime) == 1:
        asserts.equals(env, not ctx.attr.staged, runtime[0] == scope, "runtime placement preserves the original compiler File independently")
        pairs = [pair for owner in owners if hasattr(owner, "runtime_scopes") for pair in owner.runtime_scopes]
        asserts.true(env, (scope, runtime[0]) in pairs, "scope provenance ties the exact original File to the live runtime File")
    asserts.false(env, scope in info.sources.to_list(), "scope metadata is not a source root")
    asserts.equals(env, None, info.manifest, "an inherited scope is not the consumer's publication manifest")
    asserts.true(env, any([scope in owner.type_inputs.to_list() and scope in owner.files.to_list() for owner in owners]), "compiler owners retain the original scope File")
    asserts.equals(env, [], [pair for owner in owners for pair in owner.runtime_files if pair[0] == scope], "scope metadata is not a module endpoint")
    checked = []
    for action in runnable_actions(env):
        if action.mnemonic in ["TsConfig", "TsgoCheck", "TsgoDeclare"]:
            asserts.true(env, scope in action.inputs.to_list(), action.mnemonic + " retains original scope bytes")
            if action.mnemonic == "TsConfig":
                asserts.false(env, scope.path in action.argv, "scope metadata is not a configured source root")
            checked.append(action.mnemonic)
    asserts.true(env, "TsConfig" in checked and "TsgoCheck" in checked, "the emitted program still configures and checks")
    return analysistest.end(env)

scope_inputs_oxc_test = analysistest.make(
    _scope_inputs_impl,
    attrs = {
        "scope": attr.string(mandatory = True),
        "scope_source": attr.bool(),
        "staged": attr.bool(),
        "compiler_only": attr.bool(),
    },
    config_settings = {str(Label("//ts:declarations")): "oxc"},
    extra_target_under_test_aspects = [runnable_action_aspect],
)

scope_inputs_tsgo_test = analysistest.make(
    _scope_inputs_impl,
    attrs = {
        "scope": attr.string(mandatory = True),
        "scope_source": attr.bool(),
        "staged": attr.bool(),
        "compiler_only": attr.bool(),
    },
    config_settings = {str(Label("//ts:declarations")): "tsgo"},
    extra_target_under_test_aspects = [runnable_action_aspect],
)

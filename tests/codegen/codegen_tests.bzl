"""Codegen ownership, editor provenance, and custom TsInfo compatibility."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//ts:defs.bzl", "TsInfo")

def _legacy_ts_info_impl(ctx):
    info = ctx.attr.dep[TsInfo]
    label = str(ctx.label)
    owners = depset([struct(
        label = label[2:] if label.startswith("@@//") else label,
        files = record.files,
        declarations = record.declarations,
        type_inputs = record.type_inputs,
        importers = record.importers,
    ) for record in info.owners.to_list()], order = "postorder")
    return [ctx.attr.dep[DefaultInfo], TsInfo(
        js = info.js,
        js_maps = info.js_maps,
        runtime_sources = info.runtime_sources,
        runtime_source_owners = info.runtime_source_owners,
        transitive_runtime_sources = info.transitive_runtime_sources,
        declarations = info.declarations,
        data = info.data,
        manifest = info.manifest,
        sources = info.sources,
        tsconfig = info.tsconfig,
        transitive_js = info.transitive_js,
        transitive_js_maps = info.transitive_js_maps,
        transitive_data = info.transitive_data,
        transitive_es_twins = info.transitive_es_twins,
        npm_packages = info.npm_packages,
        npm_files = info.npm_files,
        owners = owners,
    )]

legacy_ts_info = rule(
    implementation = _legacy_ts_info_impl,
    attrs = {"dep": attr.label(mandatory = True, providers = [TsInfo])},
)

def _legacy_owner_consumer_impl(ctx):
    env = analysistest.begin(ctx)
    records = ctx.attr.dep[TsInfo].owners.to_list()
    asserts.equals(env, 1, len(records))
    for record in records:
        asserts.false(env, hasattr(record, "source_files"))
        asserts.false(env, hasattr(record, "generated_inputs"))
    actions = analysistest.target_actions(env)
    for mnemonic in ["TsConfig", "TsgoCheck"]:
        selected = [action for action in actions if action.mnemonic == mnemonic]
        asserts.equals(env, 1, len(selected), "ordinary analysis retains " + mnemonic)
    checks = [action for action in actions if action.mnemonic == "TsgoCheck"]
    for check in checks:
        for file in ctx.attr.dep[TsInfo].declarations.to_list() + ctx.attr.dep[TsInfo].data.to_list():
            asserts.true(env, file in check.inputs.to_list(), "ordinary checking retains custom declarations and data")
    asserts.equals(env, [], [action for action in actions if action.mnemonic == "TsIdeProject"], "unknown provenance must not produce an editor projection")
    return analysistest.end(env)

legacy_owner_consumer_test = analysistest.make(
    _legacy_owner_consumer_impl,
    attrs = {"dep": attr.label(mandatory = True, providers = [TsInfo])},
)

def _legacy_owner_refresh_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "custom TsInfo dependency owners lack editor input provenance")
    asserts.expect_failure(env, "Did you mean to use an authored editor project without generated_sources?")
    asserts.expect_failure(env, "Ordinary builds and type checking remain supported")
    return analysistest.end(env)

legacy_owner_refresh_test = analysistest.make(_legacy_owner_refresh_impl, expect_failure = True)

def _editor_owner_provenance_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[TsInfo]
    records = info.owners.to_list()
    asserts.equals(env, 1, len(records))
    for record in records:
        authored = record.source_files.to_list()
        generated = record.generated_inputs.to_list()
        asserts.equals(env, sorted([file.path for file in ctx.files.authored]), sorted([file.path for file in authored]))
        asserts.equals(env, ctx.attr.generated, sorted([file.basename for file in generated]))
        for file in authored:
            asserts.true(env, file.is_source, "editor authored inputs retain their checkout identity")
        for file in generated:
            asserts.false(env, file.is_source, "only generated artifacts exclude checkout identities")
        for file in info.data.to_list():
            asserts.false(env, file in generated, "staging authored data does not make its origin generated")
    return analysistest.end(env)

editor_owner_provenance_test = analysistest.make(
    _editor_owner_provenance_impl,
    attrs = {
        "authored": attr.label_list(allow_files = True),
        "generated": attr.string_list(),
    },
)

def _editor_dependency_inputs_impl(ctx):
    env = analysistest.begin(ctx)
    declarations = ctx.attr.emitted[TsInfo].declarations.to_list()
    npm = analysistest.target_under_test(env)[TsInfo].npm_files.to_list()
    asserts.true(env, bool(declarations), "fixture must emit declarations")
    asserts.true(env, bool(npm), "fixture must carry npm inputs")
    for mnemonic in ["TsConfig", "TsIdeProject"]:
        actions = [action for action in analysistest.target_actions(env) if action.mnemonic == mnemonic]
        asserts.equals(env, 1, len(actions), mnemonic)
        for action in actions:
            inputs = action.inputs.to_list()
            asserts.false(env, ctx.file.generated_source in inputs, mnemonic + " must not read the dependency's original generated source")
            for file in ctx.files.configs:
                asserts.true(env, file in inputs, mnemonic + " retains the generated config chain")
            if mnemonic != "TsIdeProject":
                continue
            for file in ctx.files.authored:
                asserts.true(env, file in inputs, "editor retains the dependency's authored input " + file.short_path)
                asserts.true(env, "-source=" + file.path in action.argv, "editor stages authored identity " + file.short_path)
                asserts.false(env, "-editor-generated-file=" + file.short_path in action.argv, "authored data must not exclude its checkout identity")
            for file in [ctx.file.generated_source] + declarations:
                asserts.true(env, "-editor-generated-file=" + file.short_path in action.argv, "editor excludes the generated checkout identity " + file.short_path)
            for file in declarations + npm:
                asserts.true(env, file in inputs, "editor retains canonical declarations and npm inputs: " + file.short_path)
    return analysistest.end(env)

editor_dependency_inputs_test = analysistest.make(
    _editor_dependency_inputs_impl,
    attrs = {
        "authored": attr.label_list(allow_files = True),
        "configs": attr.label_list(allow_files = True),
        "emitted": attr.label(mandatory = True, providers = [TsInfo]),
        "generated_source": attr.label(mandatory = True, allow_single_file = True),
    },
)

def _editor_refresh_runfiles_impl(ctx):
    env = analysistest.begin(ctx)
    runfiles = analysistest.target_under_test(env)[DefaultInfo].default_runfiles.files.to_list()
    npm = ctx.attr.program[TsInfo].npm_files.to_list()
    asserts.true(env, bool(npm), "fixture must carry npm runfiles")
    for file in npm + ctx.files.configs:
        asserts.true(env, file in runfiles, "refresh must materialize " + file.short_path)
    return analysistest.end(env)

editor_refresh_runfiles_test = analysistest.make(
    _editor_refresh_runfiles_impl,
    attrs = {
        "configs": attr.label_list(allow_files = True),
        "program": attr.label(mandatory = True, providers = [TsInfo]),
    },
)

def _outs_codegen_providers_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)

    asserts.true(
        env,
        TsInfo in target,
        "an outs ts_codegen provides TsInfo, so it can be a dep",
    )
    if TsInfo not in target:
        return analysistest.end(env)
    info = target[TsInfo]

    # A `.d.ts` out is a declaration a consumer's tsconfig `types` names; a
    # `.ts` out is a source for a consumer's srcs and travels in neither.
    asserts.equals(
        env,
        ["generated-globals.d.ts"],
        [f.basename for f in info.declarations.to_list()],
        "the declaration outs",
    )
    records = info.owners.to_list()
    asserts.equals(env, 1, len(records), "a codegen has no deps: one record")
    for record in records:
        asserts.equals(
            env,
            [f.basename for f in info.declarations.to_list()],
            [f.basename for f in record.declarations.to_list()],
            "the record carries the declaration outs",
        )
    asserts.equals(env, [], info.js.to_list(), "no JavaScript out")
    asserts.equals(env, [], info.npm_packages.to_list(), "no npm closure")
    return analysistest.end(env)

outs_codegen_providers_test = analysistest.make(_outs_codegen_providers_impl)

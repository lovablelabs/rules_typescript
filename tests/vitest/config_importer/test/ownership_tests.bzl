load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//tests:runnable_actions.bzl", "runnable_action_aspect", "runnable_actions")
load("//tests/workers_nested/test:wrangler_config_tests.bzl", "config_pool_action_test")
load("//ts:defs.bzl", "ts_compile", "ts_test")
load("//ts/private:providers.bzl", "NodeModulesInfo", "NpmLinkInfo")

def nested_config_pool_tests():
    config_importer = "//tests/vitest/config_importer/config/pool:node_modules"
    test_importer = "//tests/workers_nested/test:node_modules"
    other_importer = "//tests/workers_nested/test:different_pool_importer"
    for name, importer, owner, failure in [
        ("nested_config_pool", test_importer, config_importer, ""),
        ("nested_config_other_pool", other_importer, config_importer, ""),
        ("unlinked_config_pool_owner", other_importer, test_importer, "does not link @cloudflare/vitest-pool-workers"),
    ]:
        ts_test(
            name = name + "_test",
            srcs = ["workers.test.ts"],
            config = "//tests/vitest/config_importer/config:workers.config.mjs",
            config_srcs = ["//tests/vitest/config_importer/config/pool:setup.mjs"],
            config_node_modules = [config_importer],
            coverage_provider = "istanbul",
            node_modules = importer,
            # Synthetic and expected-failure subjects cannot run as valid packages; analysis tests exercise them.
            tags = ["manual"] if failure or importer == other_importer else [],
            tsconfig = "//tests/workers_nested:worker_tsconfig",
            workers_pool = owner,
            wrangler_config = "//tests/vitest/config_importer/config:wrangler.jsonc",
            deps = [
                "//tests/workers_nested:source_worker",
                "@npm_workers//:vitest",
                "@npm_workers//:vitest_coverage-istanbul",
                "@npm_workers//:zod",
            ],
        )
        if failure:
            config_rejected_test(
                name = name + "_analysis_test",
                message = failure,
                target_under_test = ":" + name + "_test",
            )
        else:
            config_pool_action_test(
                name = name + "_action_test",
                config_importer = config_importer,
                other_importer = importer if importer == other_importer else None,
                target_under_test = ":" + name + "_test",
            )

    member = ":node_modules/@cloudflare/vitest-pool-workers"
    for explicit in [False, True]:
        name = "explicit_member_pool" if explicit else "legacy_member_pool"
        ts_test(
            name = name + "_test",
            srcs = ["workers.test.ts"],
            config = "//tests/workers_nested:vitest_config",
            config_node_modules = ["//tests/workers_nested:node_modules"] + ([member] if explicit else []),
            coverage_provider = "istanbul",
            node_modules = test_importer if explicit else ":member_importer",
            tsconfig = "//tests/workers_nested:worker_tsconfig",
            workers_pool = member if explicit else None,
            wrangler_config = "//tests/workers_nested:wrangler.jsonc",
            deps = [
                "//tests/workers_nested:source_worker",
                "@npm_workers//:vitest",
                "@npm_workers//:vitest_coverage-istanbul",
                "@npm_workers//:zod",
            ] + ([] if explicit else [member]),
        )
        config_pool_action_test(
            name = name + "_action_test",
            config_importer = member,
            other_importer = "//tests/workers:node_modules",
            target_under_test = ":" + name + "_test",
        )

    root_importer = "//tests/workers:node_modules"
    for name, importer, dep, owner, absent in [
        ("legacy_transitive_member_pool", ":member_importer", member, member, root_importer),
        ("legacy_unstaged_nearer_pool", config_importer, "@npm_workers//:cloudflare_vitest-pool-workers", root_importer, config_importer),
    ]:
        ts_compile(
            name = name + "_dependency",
            testonly = True,
            srcs = ["pool_dependency.ts"],
            node_modules = ":member_importer" if dep == member else root_importer,
            deps = [dep],
        )
        ts_test(
            name = name + "_test",
            srcs = ["workers.test.ts"],
            config = "//tests/workers_nested:vitest_config",
            config_node_modules = ["//tests/workers_nested:node_modules"],
            coverage_provider = "istanbul",
            node_modules = importer,
            tsconfig = "//tests/workers_nested:worker_tsconfig",
            wrangler_config = "//tests/workers_nested:wrangler.jsonc",
            deps = [
                ":" + name + "_dependency",
                "//tests/workers_nested:source_worker",
                "@npm_workers//:vitest",
                "@npm_workers//:vitest_coverage-istanbul",
                "@npm_workers//:zod",
            ],
        )
        _legacy_pool_action_test(
            name = name + "_action_test",
            absent_importer = absent,
            importers = [importer, root_importer],
            pool_owner = owner,
            target_under_test = ":" + name + "_test",
        )

def _legacy_pool_action_impl(ctx):
    env = analysistest.begin(ctx)
    patches = [a for a in runnable_actions(env) if a.mnemonic == "WranglerTestConfig"]
    asserts.equals(env, 1, len(patches))
    if patches:
        action = patches[0]
        directories = [action.argv[i + 1] for i in range(len(action.argv) - 1) if action.argv[i] == "--node-modules"]
        asserts.equals(env, [i[NodeModulesInfo].dir for i in ctx.attr.importers], directories, "legacy pool lookup must retain importer order")
        owner = ctx.attr.pool_owner
        pool = owner[NpmLinkInfo] if NpmLinkInfo in owner else owner[NodeModulesInfo].links["@cloudflare/vitest-pool-workers"]
        inputs = action.inputs.to_list()
        runfiles = analysistest.target_under_test(env)[DefaultInfo].default_runfiles.files.to_list()
        for file in [pool.link] + pool.store.transitive.to_list():
            asserts.true(env, file in inputs, "transitive runtime pool is missing from preparation: " + file.path)
            asserts.true(env, file in runfiles, "preparation pool is missing from runtime: " + file.path)
        absent = ctx.attr.absent_importer[NodeModulesInfo].links["@cloudflare/vitest-pool-workers"].link
        asserts.false(env, absent in runfiles, "the fixture must leave the declared pool unstaged")
        asserts.false(env, absent in inputs, "preparation must not introduce an unstaged declared pool")
    return analysistest.end(env)

_legacy_pool_action_test = analysistest.make(
    _legacy_pool_action_impl,
    extra_target_under_test_aspects = [runnable_action_aspect],
    attrs = {
        "absent_importer": attr.label(mandatory = True, providers = [NodeModulesInfo]),
        "importers": attr.label_list(providers = [NodeModulesInfo]),
        "pool_owner": attr.label(mandatory = True, providers = [[NodeModulesInfo], [NpmLinkInfo]]),
    },
)

def _config_rejected_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, ctx.attr.message)
    return analysistest.end(env)

config_rejected_test = analysistest.make(
    _config_rejected_impl,
    attrs = {"message": attr.string(mandatory = True)},
    expect_failure = True,
)

def config_importer_tests():
    common = {
        "srcs": ["importer.test.ts"],
        "node_modules": ":node_modules",
        "deps": ["@npm//:minimatch_9_0_9", "@npm//:types_node", "@npm//:vitest"],
    }
    config_importer = "//tests/vitest/config_importer/config:node_modules"
    for name, attr in [
        ("config_importer_test", "config_node_modules"),
        ("authored_data_config_importer_test", "data"),
    ]:
        ts_test(
            name = name,
            config = "//tests/vitest/config_importer/config:vitest_config",
            **(common | {attr: [config_importer]})
        )
    for name, attr, value in [
        ("config_importers", "config_node_modules", [config_importer]),
        ("workers_pool", "workers_pool", "//tests/vitest/config_importer/config/pool:node_modules"),
    ]:
        # The deliberately invalid subject runs only through its analysis test.
        ts_test(
            name = name + "_with_node_test",
            srcs = [],
            runner = "//ts/runners:node_test",
            tags = ["manual"],
            **{attr: value}
        )
        config_rejected_test(
            name = name + "_rejected_test",
            message = "the node:test runner reads none of " + attr + ".",
            target_under_test = ":" + name + "_with_node_test",
        )

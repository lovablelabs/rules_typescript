"""A ts_test runs in the importer chain its tsgo action resolved against: no
tree is built, the launcher is handed the importer's node_modules directory,
and a dep's package reaches the runfiles through the dep's npm_files."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//tests:runnable_actions.bzl", "RunnableActionOwnerInfo", "runnable_action_aspect")

def _only_output(action, suffix):
    outputs = action.outputs.to_list()
    if len(outputs) == 1 and outputs[0].basename.endswith(suffix):
        return outputs[0]
    return None

def _runtime_is_the_chain_impl(ctx):
    env = unittest.begin(ctx)
    actions = ctx.attr.test[RunnableActionOwnerInfo].actions
    trees = [a for a in actions if a.mnemonic == "NodeModulesTree"]
    tsgo = [a for a in actions if a.mnemonic == "TsgoCheck"]
    launchers = [a for a in actions if _only_output(a, "_test_launcher.json")]
    asserts.equals(env, [], trees, "no tree is built per target")
    asserts.equals(env, 1, len(tsgo), "one TsgoCheck action")
    asserts.equals(env, 1, len(launchers), "one launcher config")
    if len(tsgo) == 1 and len(launchers) == 1:
        importer = ctx.bin_dir.path + "/tests/npm/node_modules"
        asserts.equals(
            env,
            ["-node_modules=" + importer],
            [a for a in tsgo[0].argv if a.startswith("-node_modules=")],
            "tsgo resolves through the importer's node_modules",
        )
        asserts.true(
            env,
            len([
                f
                for f in tsgo[0].inputs.to_list()
                if f.path == importer + "/zod"
            ]) == 1,
            "zod, declared by the ts_compile dep alone, is staged through " +
            "the dep's npm_files",
        )
        content = launchers[0].content
        asserts.equals(
            env,
            ["{}/tests/npm/node_modules".format(ctx.workspace_name)],
            json.decode(content)["vitest"]["node_modules"],
            "the launcher runs the tests in the importer's node_modules: " +
            content,
        )
    return unittest.end(env)

runtime_is_the_chain_test = unittest.make(
    _runtime_is_the_chain_impl,
    attrs = {"test": attr.label(aspects = [runnable_action_aspect])},
)

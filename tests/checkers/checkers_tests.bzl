"""--//ts:checkers=N puts --checkers N on TsgoCheck's tsgo command line and
-checkers=N on TsgoDeclare's tsaction emit, which hands tsgo --checkers N; at
0, the default, tsgo's thread count is its own. The cpu:N requirement the same
actions carry is aquery's to show: Starlark's Action has no execution_info."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")

_TSGO = ["TsgoCheck", "TsgoDeclare"]

def _tsgo_actions(env):
    return {
        action.mnemonic: action
        for action in analysistest.target_actions(env)
        if action.mnemonic in _TSGO
    }

def _checkers(mnemonic, argv):
    if mnemonic == "TsgoDeclare":
        values = [arg[len("-checkers="):] for arg in argv if arg.startswith("-checkers=")]
        return ",".join(values) if values else None
    if "--checkers" not in argv:
        return None
    return argv[argv.index("--checkers") + 1]

def _sixteen_impl(ctx):
    env = analysistest.begin(ctx)
    actions = _tsgo_actions(env)
    for mnemonic in _TSGO:
        asserts.true(env, mnemonic in actions, "the leaf runs " + mnemonic)
        if mnemonic in actions:
            asserts.equals(
                env,
                "16",
                _checkers(mnemonic, actions[mnemonic].argv),
                mnemonic + " passes --checkers 16",
            )
    return analysistest.end(env)

checkers_sixteen_test = analysistest.make(
    _sixteen_impl,
    config_settings = {str(Label("//ts:checkers")): 16},
)

def _default_impl(ctx):
    env = analysistest.begin(ctx)
    actions = _tsgo_actions(env)
    for mnemonic in _TSGO:
        asserts.true(env, mnemonic in actions, "the leaf runs " + mnemonic)
        if mnemonic in actions:
            asserts.equals(
                env,
                None,
                _checkers(mnemonic, actions[mnemonic].argv),
                mnemonic + " leaves tsgo's thread count alone",
            )
    return analysistest.end(env)

checkers_default_test = analysistest.make(_default_impl)

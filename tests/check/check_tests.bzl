"""Under a check_config naming a binary, TsgoCheck runs it with its args and
then tsgo's command line, and TsgoDeclare and TsIdeProject keep tsgo; under
//ts:tsgo_check every one of them runs tsgo."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")

_CHECK = str(Label("//ts:check"))
_TSGO = ["TsgoCheck", "TsgoDeclare", "TsIdeProject"]

def _tsgo_actions(env):
    return {
        action.mnemonic: action
        for action in analysistest.target_actions(env)
        if action.mnemonic in _TSGO
    }

def _compiler(action):
    argv = action.argv
    if action.mnemonic == "TsgoDeclare":
        return [arg[len("-tsgo="):] for arg in argv if arg.startswith("-tsgo=")][0]
    return argv[argv.index("--") + 1]

def _checker_impl(ctx):
    env = analysistest.begin(ctx)
    actions = _tsgo_actions(env)
    for mnemonic in _TSGO:
        asserts.true(env, mnemonic in actions, "the leaf runs " + mnemonic)
    if "TsgoCheck" in actions:
        check = actions["TsgoCheck"]
        argv = check.argv
        at = argv.index("--")
        asserts.true(env, argv[at + 1].endswith("/tests/check/fake_checker"), "TsgoCheck runs the checker: " + argv[at + 1])
        asserts.equals(env, ["tsc", "--project"], argv[at + 2:at + 4], "the checker's args, then tsgo's command line")
        for flag in ["--noEmit", "--explainFiles"]:
            asserts.true(env, flag in argv[at:], flag + " reaches the checker")
        inputs = [f.basename for f in check.inputs.to_list()]
        asserts.true(env, "fake_checker" in inputs, "the checker is an input")
    for mnemonic in ["TsgoDeclare", "TsIdeProject"]:
        if mnemonic in actions:
            compiler = _compiler(actions[mnemonic])
            asserts.false(env, compiler.endswith("fake_checker"), mnemonic + " keeps tsgo: " + compiler)
    return analysistest.end(env)

checker_test = analysistest.make(
    _checker_impl,
    config_settings = {_CHECK: str(Label("//tests/check:fake"))},
)

def _tsgo_checks_impl(ctx):
    env = analysistest.begin(ctx)
    actions = _tsgo_actions(env)
    asserts.true(env, "TsgoCheck" in actions, "the leaf runs TsgoCheck")
    if "TsgoCheck" in actions:
        compiler = _compiler(actions["TsgoCheck"])
        asserts.false(env, compiler.endswith("fake_checker"), "TsgoCheck runs tsgo: " + compiler)
        argv = actions["TsgoCheck"].argv
        asserts.equals(env, "--project", argv[argv.index("--") + 2], "nothing between tsgo and its command line")
    return analysistest.end(env)

tsgo_checks_test = analysistest.make(
    _tsgo_checks_impl,
    config_settings = {_CHECK: str(Label("//ts:tsgo_check"))},
)

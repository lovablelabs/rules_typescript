"""ts_test compiles its srcs with ts_compile's actions, less the declaration
emit: over the same srcs, deps and tsconfig, the two register the same
TsConfig and TsgoCheck actions, argv for argv once the target's package and
name are put aside, the vitest test's TsEmit is the compile's ES-modules emit
-- -es_modules, oxc's inputs alone (docs/rules/ts-test.md § Runners) -- and
the compile's TsgoDeclare has no counterpart (§ The Test's Program)."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//tests:runnable_actions.bzl", "RunnableActionOwnerInfo", "runnable_action_aspect")

_COMPILE_MNEMONICS = ["TsConfig", "TsEmit", "TsgoDeclare", "TsgoCheck"]

_TEST_MNEMONICS = ["TsConfig", "TsEmit", "TsgoCheck"]

_TSGO_INPUTS = (
    "-tsconfig=",
    "-source=",
    "-node_modules=",
    "-inherit_node_modules=",
    "-scratch=",
    "-tsgo=",
)

def _es_modules_emit(argv):
    """The compile's TsEmit argv as the vitest runner's program registers it."""
    flags = [
        a
        for a in argv[2:]
        if a.startswith("-") and not a.startswith(_TSGO_INPUTS)
    ]
    srcs = [a for a in argv[2:] if not a.startswith("-")]
    return argv[:2] + flags + ["-es_modules"] + srcs

def _compile_argv(target):
    """Each compile action's argv, the target's package and name abstracted."""
    owner = target[RunnableActionOwnerInfo]
    out = {}
    for action in owner.actions:
        if action.mnemonic not in _COMPILE_MNEMONICS:
            continue
        out[action.mnemonic] = [
            arg.replace(owner.label.package, "<package>").replace(
                owner.label.name,
                "<name>",
            )
            for arg in action.argv
        ]
    return out

def _compile_parity_impl(ctx):
    env = unittest.begin(ctx)
    compile = _compile_argv(ctx.attr.compile)
    test = _compile_argv(ctx.attr.test)
    asserts.equals(
        env,
        sorted(_COMPILE_MNEMONICS),
        sorted(compile.keys()),
        "the ts_compile registers the four compile actions",
    )
    asserts.equals(
        env,
        sorted(_TEST_MNEMONICS),
        sorted(test.keys()),
        "the ts_test registers the ts_compile's, less TsgoDeclare",
    )
    for mnemonic in _TEST_MNEMONICS:
        want = compile.get(mnemonic)
        if mnemonic == "TsEmit":
            want = _es_modules_emit(want)
        asserts.equals(env, want, test.get(mnemonic), mnemonic + " argv")
    return unittest.end(env)

compile_parity_test = unittest.make(
    _compile_parity_impl,
    attrs = {
        "compile": attr.label(aspects = [runnable_action_aspect]),
        "test": attr.label(aspects = [runnable_action_aspect]),
    },
)

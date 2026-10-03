"""A ts_compile's declarations are the `declarations` output group and
TsInfo.declarations, never a default output: a leaf runs TsgoCheck alone,
TsgoDeclare runs when a dependent's compile reads the .d.ts."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//tests:runnable_actions.bzl", "runnable_action_aspect", "runnable_actions")
load("//ts:defs.bzl", "TsInfo")

_PKG = "tests/declarations/"

def _rel(f):
    return f.path[f.path.find(_PKG) + len(_PKG):]

def _action(env, mnemonic, actions = None):
    for action in actions if actions != None else analysistest.target_actions(env):
        if action.mnemonic == mnemonic:
            return action
    return None

def _leaf_declares_on_demand_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    groups = target[OutputGroupInfo]

    asserts.equals(
        env,
        ["lib.js", "lib.js.map"],
        sorted([_rel(f) for f in target[DefaultInfo].files.to_list()]),
        "the default outputs: the .js and its map, no declaration",
    )
    asserts.equals(
        env,
        ["lib.d.ts"],
        [_rel(f) for f in groups.declarations.to_list()],
        "the declarations output group",
    )
    asserts.equals(
        env,
        ["lib.d.ts"],
        [_rel(f) for f in target[TsInfo].declarations.to_list()],
        "TsInfo.declarations",
    )

    check = _action(env, "TsgoCheck")
    asserts.true(env, check != None, "the leaf runs TsgoCheck")
    if check != None:
        argv = check.argv
        asserts.true(env, "--noEmit" in argv, "the check emits nothing")
        asserts.true(env, "--explainFiles" in argv, "the check lists the edges")
        asserts.equals(
            env,
            1,
            len([a for a in argv if a.startswith("-check=")]),
            "the check reads the ownership manifest",
        )
        asserts.equals(
            env,
            ["lib.tscheck"],
            [_rel(f) for f in check.outputs.to_list()],
            "the check's output is its stamp",
        )
        asserts.true(
            env,
            check.outputs.to_list()[0] in groups._validation.to_list(),
            "the stamp is in _validation",
        )

    declare = _action(env, "TsgoDeclare")
    asserts.true(env, declare != None, "the leaf registers TsgoDeclare")
    if declare != None:
        argv = declare.argv
        asserts.equals(
            env,
            ["lib.d.ts"],
            [_rel(f) for f in declare.outputs.to_list()],
            "the declare's outputs are the declarations",
        )
        asserts.true(env, "emit" in argv and "-declarations_only" in argv, "the declaration action requests the checked tsgo program")
        asserts.equals(env, 1, len([arg for arg in argv if arg.startswith("-tsgo=")]), "one tsgo compiler")
        asserts.equals(env, [], [arg for arg in argv if arg.startswith("-oxc=") or arg == "-declarations"], "no isolated declaration backend")
        out_dirs = [arg for arg in argv if arg.startswith("-out_dir=")]
        asserts.equals(env, 1, len(out_dirs), "one output directory")
        if out_dirs:
            asserts.true(env, out_dirs[0].endswith("/tests/declarations"), "declarations stay in the package output directory")
        asserts.equals(env, ["-root=tests/declarations"], [arg for arg in argv if arg.startswith("-root=")], "single-package source root")
        asserts.true(env, "--explainFiles" not in argv, "no listing")
        asserts.equals(
            env,
            [],
            [a for a in argv if a.startswith(("-check=", "-stamp="))],
            "the declare checks no ownership and writes no stamp",
        )
    return analysistest.end(env)

leaf_declares_on_demand_test = analysistest.make(_leaf_declares_on_demand_impl)

def _check_reads_the_dep_declarations_impl(ctx):
    env = analysistest.begin(ctx)
    dep_dts = ctx.attr.dep[TsInfo].declarations.to_list()
    asserts.equals(
        env,
        ["lib.d.ts"],
        [_rel(f) for f in dep_dts],
        "the dep's declarations",
    )

    for mnemonic in ["TsgoCheck", "TsgoDeclare", "TsConfig", "TsEmit"]:
        action = _action(env, mnemonic)
        asserts.true(env, action != None, "the dependent runs " + mnemonic)
        if action != None:
            inputs = action.inputs.to_list()
            for f in dep_dts:
                asserts.equals(
                    env,
                    mnemonic in ["TsgoCheck", "TsgoDeclare"],
                    f in inputs,
                    "{} dependency content edge for {}".format(mnemonic, _rel(f)),
                )
    return analysistest.end(env)

check_reads_the_dep_declarations_test = analysistest.make(
    _check_reads_the_dep_declarations_impl,
    attrs = {
        "dep": attr.label(
            mandatory = True,
            doc = "The ts_compile the target under test depends on.",
        ),
    },
)

def _compiler_inputs_remain_type_only_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    actions = runnable_actions(env)
    for extra in ctx.files.compiler_inputs:
        for mnemonic in ["TsConfig", "TsgoCheck"]:
            action = _action(env, mnemonic, actions)
            asserts.true(env, action != None, mnemonic + " exists")
            if action != None:
                asserts.true(env, extra in action.inputs.to_list(), mnemonic + " retains compiler metadata")
                if mnemonic == "TsConfig":
                    asserts.true(env, extra.path not in action.argv, "metadata is not an explicit source root")
        if TsInfo in target:
            declare = _action(env, "TsgoDeclare", actions)
            asserts.true(env, declare != None, "emitted declarations retain compiler inputs")
            if declare != None:
                asserts.true(env, extra in declare.inputs.to_list(), "declaration emit retains compiler metadata")
            info = target[TsInfo]
            for files in [info.data, info.transitive_data, info.js, info.declarations, info.sources]:
                asserts.true(env, extra not in files.to_list(), "compiler metadata acquired a runtime, declaration or source-root role")
        asserts.true(env, extra not in target[DefaultInfo].files.to_list(), "compiler metadata became a default output")
    return analysistest.end(env)

compiler_inputs_remain_type_only_test = analysistest.make(
    _compiler_inputs_remain_type_only_impl,
    attrs = {"compiler_inputs": attr.label_list(allow_files = True)},
    extra_target_under_test_aspects = [runnable_action_aspect],
)

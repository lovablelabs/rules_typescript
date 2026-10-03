"""Analysis-time proof of the TsLint action under each lint_config.

The action runs the configured tool inside tsgo's program layout, with config
imports declared separately from the linted sources. The real linter is
//tests/lint_real's oxlint, through the root module's tag.
"""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//tests:runnable_actions.bzl", "runnable_action_aspect", "runnable_action_label", "runnable_actions")

_LINT = str(Label("//ts:lint"))
_PKG = "tests/lint"

def _tslint_actions(env):
    return [
        action
        for action in runnable_actions(env)
        if action.mnemonic == "TsLint"
    ]

def _lints_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    actions = _tslint_actions(env)
    asserts.equals(env, 1, len(actions), "TsLint actions")
    if len(actions) != 1:
        return analysistest.end(env)
    action = actions[0]
    argv = action.argv

    stamps = action.outputs.to_list()
    asserts.equals(
        env,
        [runnable_action_label(env).name + ".tslint"],
        [f.basename for f in stamps],
        "the stamp",
    )
    asserts.true(
        env,
        stamps[0] in target[OutputGroupInfo]._validation.to_list(),
        "the stamp is in _validation",
    )

    asserts.equals(env, "tsgo", argv[1], "the shared program runner")
    linter = argv[argv.index("--") + 1]
    asserts.true(
        env,
        linter.endswith("/" + _PKG + "/fake_linter"),
        "the linter is the config's binary: " + linter,
    )
    asserts.equals(
        env,
        ctx.attr.fail_on_warnings,
        "--max-warnings=0" in argv,
        "--max-warnings=0 iff fail_on_warnings",
    )
    asserts.equals(
        env,
        ctx.attr.config != "",
        "--config" in argv,
        "--config iff the config names a file",
    )
    want = [_PKG + "/" + f for f in ctx.attr.linted]
    asserts.equals(env, want, argv[-len(want):], "workspace-relative linted sources")
    inputs = [f.short_path for f in action.inputs.to_list()]
    for path in want:
        asserts.true(env, path in inputs, path + " is an input")
    asserts.true(env, runnable_action_label(env).name + ".tsconfig.json" in [f.basename for f in action.inputs.to_list()])
    configs = [f.path for f in action.inputs.to_list() if f.basename == runnable_action_label(env).name + ".tsconfig.json"]
    asserts.true(env, "-discover-tsconfig=" + configs[0] in argv, "automatic discovery uses the generated compiler program")
    asserts.true(env, "types.d.ts" in [f.basename for f in action.inputs.to_list()], "program declarations reach lint")
    if ctx.attr.config:
        config = _PKG + "/" + ctx.attr.config
        asserts.equals(env, config, argv[argv.index("--config") + 1])
        for path in [config, _PKG + "/plugin.mjs", _PKG + "/plugin-options.json"]:
            asserts.true(env, path in inputs, path + " is an input")
            asserts.true(env, "-copy=" + path in argv, path + " retains module resolution inside the program")
        asserts.true(env, any([path.endswith("/node_modules/oxlint") for path in inputs]), "config npm imports are declared")
        asserts.true(env, any([arg.startswith("-node_modules=") for arg in argv]), "config importer reaches dependency-free programs")
        tools = [f for f in action.inputs.to_list() if f.basename == "auxiliary_tool"]
        asserts.equals(env, 1, len(tools), "auxiliary executable is an action input")
        if tools:
            asserts.true(env, "-tool-env=LINT_AUXILIARY=" + tools[0].path in argv, "environment names the declared executable")
        asserts.true(env, "--type-aware" in argv)
        asserts.true(env, "--tsconfig=" + configs[0] in argv)
    if target.label.name == "clean_test":
        asserts.true(env, any(["/node_modules/vitest" in path for path in inputs]), "npm inputs reach lint")
    return analysistest.end(env)

_LINTS_ATTRS = {
    "linted": attr.string_list(
        doc = "The package-relative srcs the action lints, in order.",
    ),
    "config": attr.string(
        doc = "The package-relative config file passed as --config, or \"\".",
    ),
    "fail_on_warnings": attr.bool(),
}

strict_lint_test = analysistest.make(
    _lints_impl,
    attrs = _LINTS_ATTRS,
    extra_target_under_test_aspects = [runnable_action_aspect],
    config_settings = {_LINT: str(Label("//tests/lint:strict"))},
)

loose_lint_test = analysistest.make(
    _lints_impl,
    attrs = _LINTS_ATTRS,
    extra_target_under_test_aspects = [runnable_action_aspect],
    config_settings = {_LINT: str(Label("//tests/lint:loose"))},
)

def _no_lint_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.equals(
        env,
        0,
        len(_tslint_actions(env)),
        "TsLint actions under a lint_config naming no binary",
    )
    return analysistest.end(env)

no_lint_test = analysistest.make(
    _no_lint_impl,
    config_settings = {_LINT: str(Label("//tests/lint:none"))},
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def lint_test_suite(name):
    """The three configs over :clean, the strict one over :clean_test too."""
    strict_lint_test(
        name = name + "_strict",
        target_under_test = ":clean",
        linted = ["clean.ts", "types.d.ts"],
        config = "lint.json",
        fail_on_warnings = True,
    )
    strict_lint_test(
        name = name + "_strict_ts_test",
        target_under_test = ":clean_test",
        linted = ["clean.test.ts"],
        config = "lint.json",
        fail_on_warnings = True,
    )
    strict_lint_test(
        name = name + "_declarations_only",
        target_under_test = ":types_only",
        linted = ["types.d.ts"],
        config = "lint.json",
        fail_on_warnings = True,
    )
    loose_lint_test(
        name = name + "_loose",
        target_under_test = ":clean",
        linted = ["clean.ts", "types.d.ts"],
    )
    no_lint_test(
        name = name + "_none",
        target_under_test = ":clean",
    )
    native.test_suite(
        name = name,
        tests = [
            ":" + name + "_strict",
            ":" + name + "_strict_ts_test",
            ":" + name + "_loose",
            ":" + name + "_declarations_only",
            ":" + name + "_none",
        ],
    )

def _invalid_tool_env_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, ctx.attr.expected)
    return analysistest.end(env)

invalid_tool_env_test = analysistest.make(
    _invalid_tool_env_impl,
    expect_failure = True,
    attrs = {"expected": attr.string()},
)

"""Which launched programs get the runtime tools, and what they get."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//tests:runnable_actions.bzl", "runnable_action_aspect", "runnable_actions")

_TOOLS = str(Label("//tests/runtime_tools:tools"))

_PROBE = "_main/tests/runtime_tools/probe_/probe"

def _with_tools_impl(_settings, _attr):
    return {_TOOLS: "on"}

_with_tools = transition(
    implementation = _with_tools_impl,
    inputs = [],
    outputs = [_TOOLS],
)

def _runtime_tools_test_impl(ctx):
    program = ctx.attr.program
    if type(program) == "list":
        program = program[0]
    program = program[DefaultInfo]
    launcher = program.files_to_run.executable

    # Same basename: the launcher finds its config at the runfiles root by it.
    executable = ctx.actions.declare_file(ctx.label.name + "/" + launcher.basename)
    ctx.actions.symlink(output = executable, target_file = launcher, is_executable = True)
    return [DefaultInfo(
        executable = executable,
        runfiles = ctx.runfiles(files = [executable]).merge(program.default_runfiles),
    )]

runtime_tools_test = rule(
    implementation = _runtime_tools_test_impl,
    test = True,
    attrs = {
        "program": attr.label(
            cfg = _with_tools,
            executable = True,
            mandatory = True,
            doc = "The launched program to run with //tests/runtime_tools:tools set to `on`.",
        ),
        "_allowlist_function_transition": attr.label(
            default = "@bazel_tools//tools/allowlists/function_transition_allowlist",
        ),
    },
    doc = "Runs `program` with this package's runtime tools toolchain resolving for it.",
)

def _launcher_config(env):
    configs = [
        action
        for action in runnable_actions(env)
        if action.mnemonic == "FileWrite" and [f for f in action.outputs.to_list() if f.basename.endswith("_launcher.json")]
    ]
    asserts.equals(env, 1, len(configs), "one launcher config")
    return json.decode(configs[0].content) if configs else {}

def _carries_probe(target):
    return [f for f in target[DefaultInfo].default_runfiles.files.to_list() if f.short_path == "tests/runtime_tools/probe_/probe"] != []

def _gets_tools_impl(ctx):
    env = analysistest.begin(ctx)
    config = _launcher_config(env)
    asserts.equals(env, {"RT_PROBE_TOOL": _PROBE}, config.get("tool_env"), "the launcher config names the tool by its runfiles path")
    asserts.true(env, _carries_probe(analysistest.target_under_test(env)), "the program's runfiles hold the tool")
    return analysistest.end(env)

def _skips_tools_impl(ctx):
    env = analysistest.begin(ctx)
    config = _launcher_config(env)
    asserts.equals(env, None, config.get("tool_env"), "a program outside `packages` gets no tool_env")
    asserts.false(env, _carries_probe(analysistest.target_under_test(env)), "a program outside `packages` does not depend on the tool")
    return analysistest.end(env)

def _make(impl):
    return analysistest.make(
        impl,
        config_settings = {_TOOLS: "on"},
        extra_target_under_test_aspects = [runnable_action_aspect],
    )

gets_tools_test = _make(_gets_tools_impl)

skips_tools_test = _make(_skips_tools_impl)

def _env_collision_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "env sets RT_PROBE_TOOL, which the runtime tools toolchain sets")
    return analysistest.end(env)

env_collision_test = analysistest.make(
    _env_collision_impl,
    expect_failure = True,
    config_settings = {_TOOLS: "on"},
)

def _invalid_name_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "needs an environment variable name, got \"1BAD\"")
    return analysistest.end(env)

invalid_name_test = analysistest.make(_invalid_name_impl, expect_failure = True)

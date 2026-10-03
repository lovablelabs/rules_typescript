"""Action ownership across a runnable's public target and private program."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest")

RunnableActionOwnerInfo = provider(fields = ["actions", "label", "repo_mapping"])

def _runnable_action_impl(target, ctx):
    program = getattr(ctx.rule.attr, "native_program", None)
    if program:
        return [program[RunnableActionOwnerInfo]]
    return [RunnableActionOwnerInfo(actions = target.actions, label = target.label, repo_mapping = target[DefaultInfo].files_to_run.repo_mapping_manifest)]

runnable_action_aspect = aspect(
    implementation = _runnable_action_impl,
    attr_aspects = ["native_program"],
)

def runnable_actions(env):
    return analysistest.target_under_test(env)[RunnableActionOwnerInfo].actions

def runnable_action_label(env):
    return analysistest.target_under_test(env)[RunnableActionOwnerInfo].label

def runnable_repo_mapping(env):
    return analysistest.target_under_test(env)[RunnableActionOwnerInfo].repo_mapping

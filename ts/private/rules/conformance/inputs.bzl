"""Declared tools for the Go integration owner; no model execution at analysis."""

load("//tools/launcher:launcher.bzl", "rlocation_path")

def _conformance_inputs_impl(ctx):
    workers = [int(tag.removeprefix("cpu:")) for tag in ctx.attr.reservation_tags if tag.startswith("cpu:")]
    if len(workers) != 1 or workers[0] < 1:
        fail("conformance requires one positive CPU reservation")
    java = ctx.attr.java[platform_common.ToolchainInfo].java_runtime
    executables = [file for file in java.files.to_list() if file.path == str(java.java_executable_exec_path)]
    if len(executables) != 1:
        fail("conformance requires a declared Java executable File")
    manifest = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(manifest, json.encode({
        "java": rlocation_path(ctx, executables[0]),
        "tlc": rlocation_path(ctx, ctx.file.tlc),
        "canonical": rlocation_path(ctx, ctx.executable.canonical),
        "producer": rlocation_path(ctx, ctx.executable.producer),
        "models": [rlocation_path(ctx, file) for file in ctx.files.models],
        "workers": workers[0],
    }))
    runfiles = ctx.runfiles(files = [manifest, ctx.file.tlc, ctx.executable.canonical, ctx.executable.producer] + ctx.files.models, transitive_files = java.files)
    for target in [ctx.attr.canonical, ctx.attr.producer]:
        runfiles = runfiles.merge(target[DefaultInfo].default_runfiles)
    return [DefaultInfo(files = depset([manifest]), runfiles = runfiles)]

conformance_inputs = rule(
    implementation = _conformance_inputs_impl,
    attrs = {
        "java": attr.label(mandatory = True),
        "tlc": attr.label(mandatory = True, allow_single_file = True),
        "canonical": attr.label(mandatory = True, executable = True, cfg = "target"),
        "producer": attr.label(mandatory = True, executable = True, cfg = "target"),
        "models": attr.label(mandatory = True, allow_files = True),
        "reservation_tags": attr.string_list(mandatory = True),
    },
)

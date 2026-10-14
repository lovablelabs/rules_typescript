"""Runnable targets for whichever toolchain is active.

    bazel run //ts/toolchain:oxc_resolved -- --help
    bazel run //ts/toolchain:tsgo_resolved -- --version
    bazel run //ts/toolchain:node_resolved -- --version
    bazel run //ts/toolchain:tools_resolved -- --help
    bazel run //ts/toolchain:launcher_resolved
"""

load("@toolchain_utils//toolchain:resolved.bzl", _resolved = "export")
load(
    "//ts/private:toolchain.bzl",
    "TOOLS_TOOLCHAIN_TYPE",
    "TSGO_TOOLCHAIN_TYPE",
    "get_tools_toolchain",
)

oxc_resolved = _resolved.rule(
    toolchain_type = Label("//ts/toolchain:oxc_toolchain_type"),
)

def _tsgo_resolved_impl(ctx):
    toolchain = ctx.toolchains[ctx.attr.toolchain_type.label]
    compiler = toolchain.tsgo_info.tsgo_binary
    outputs = []
    executable = None

    # RE materializes the executable symlink; each library needs an adjacent output.
    for file in toolchain.tsgo_info.files.to_list():
        out = ctx.actions.declare_file(ctx.label.name + "/" + file.basename)
        ctx.actions.symlink(
            output = out,
            target_file = file,
            is_executable = file == compiler,
        )
        outputs.append(out)
        if file == compiler:
            executable = out
    if toolchain.tsgo_info.manifest:
        manifest = ctx.actions.declare_file(ctx.label.name + "/package.json")
        ctx.actions.symlink(output = manifest, target_file = toolchain.tsgo_info.manifest)
        outputs.append(manifest)
    return [
        toolchain,
        platform_common.TemplateVariableInfo({toolchain.variable: executable.path}),
        DefaultInfo(
            executable = executable,
            files = depset([executable]),
            runfiles = ctx.runfiles(outputs, transitive_files = toolchain.tsgo_info.files),
        ),
    ]

tsgo_resolved = rule(
    doc = _resolved.doc.format(toolchain = TSGO_TOOLCHAIN_TYPE),
    implementation = _tsgo_resolved_impl,
    attrs = _resolved.attrs,
    provides = [platform_common.TemplateVariableInfo, platform_common.ToolchainInfo, DefaultInfo],
    executable = True,
    toolchains = [TSGO_TOOLCHAIN_TYPE],
)

node_resolved = _resolved.rule(
    toolchain_type = Label("//ts/toolchain:js_runtime_type"),
)

tools_resolved = _resolved.rule(
    toolchain_type = TOOLS_TOOLCHAIN_TYPE,
)

launcher_resolved = _resolved.rule(
    toolchain_type = Label("//ts/toolchain:launcher_toolchain_type"),
)

def _lcov_merger_resolved_impl(ctx):
    merger = get_tools_toolchain(ctx).lcov_merger
    executable = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(
        output = executable,
        target_file = merger,
        is_executable = True,
    )
    return [DefaultInfo(
        executable = executable,
        runfiles = ctx.runfiles([merger]),
    )]

lcov_merger_resolved = rule(
    implementation = _lcov_merger_resolved_impl,
    executable = True,
    toolchains = [TOOLS_TOOLCHAIN_TYPE],
    doc = """The tools toolchain's lcov_merger as an executable target.

Bazel's test setup reads a test rule's coverage merger from its `_lcov_merger`
attribute, so ts_test's default names this target and the binary it runs is
the toolchain's.
""",
)

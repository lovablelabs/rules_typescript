"""Executables a launched program spawns, handed to it by environment variable.

A `ts_runtime_tools` target is the toolchain behind
`//ts/toolchain:runtime_tools_type`; every rule that runs the launcher resolves
it, merges the tools' runfiles and sets each variable to its tool's absolute
path. docs/guides/runtime-tools.md.
"""

RUNTIME_TOOLS_TOOLCHAIN_TYPE = Label("//ts/toolchain:runtime_tools_type")

RuntimeToolsInfo = provider(
    doc = "The executables a `ts_runtime_tools` toolchain hands to launched programs.",
    fields = {
        "tool_env": "dict of string -> FilesToRunProvider: per environment variable, the tool its value locates.",
        "runfiles": "runfiles: every tool's executable and runfiles.",
        "packages": "list of string: npm package names; non-empty attaches the tools only to programs whose npm closure holds one of them.",
    },
)

_NAME_START = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_"
_NAME_REST = _NAME_START + "0123456789"

def tool_env_executables(tool_env):
    """Validates a label_keyed_string_dict of executables to environment names.

    Args:
        tool_env: the attribute value, target -> environment variable name.

    Returns:
        dict of environment variable name -> FilesToRunProvider.
    """
    tools = {}
    for target, name in tool_env.items():
        if not name or name[0] not in _NAME_START or any([c not in _NAME_REST for c in name.elems()]):
            fail("tool_env for {} needs an environment variable name, got {}".format(target.label, repr(name)))
        if name in tools:
            fail("tool_env assigns {} to more than one executable".format(name))
        tool = target[DefaultInfo].files_to_run
        if not tool or not tool.executable:
            fail("tool_env target {} must be executable".format(target.label))
        tools[name] = tool
    return tools

def _ts_runtime_tools_impl(ctx):
    tool_env = tool_env_executables(ctx.attr.tool_env)
    runfiles = ctx.runfiles(files = [tool.executable for tool in tool_env.values()])
    for target in ctx.attr.tool_env:
        runfiles = runfiles.merge(target[DefaultInfo].default_runfiles)
    return [platform_common.ToolchainInfo(runtime_tools = RuntimeToolsInfo(
        tool_env = tool_env,
        runfiles = runfiles,
        packages = ctx.attr.packages,
    ))]

ts_runtime_tools = rule(
    implementation = _ts_runtime_tools_impl,
    attrs = {
        "tool_env": attr.label_keyed_string_dict(
            doc = "Executable -> environment variable name. The launcher sets the variable to the executable's absolute path.",
            cfg = "target",
            mandatory = True,
        ),
        "packages": attr.string_list(
            doc = "npm package names. When set, only a program whose npm closure holds one of them gets the tools; " +
                  "the others neither carry the variables nor depend on the tools' files.",
        ),
    },
    doc = """Declares the executables launched programs spawn, as a toolchain of
`@rules_typescript//ts/toolchain:runtime_tools_type`.

    ts_runtime_tools(
        name = "compiler_tools",
        tool_env = {"//tools/compiler": "COMPILER_BINARY"},
        packages = ["@acme/compiler"],
    )

    toolchain(
        name = "compiler_toolchain",
        toolchain = ":compiler_tools",
        toolchain_type = "@rules_typescript//ts/toolchain:runtime_tools_type",
    )

The tools are built for the target platform, as the JS runtime is.
""",
)

def get_runtime_tools(ctx):
    """The resolved RuntimeToolsInfo, or None when none is registered."""
    toolchain = ctx.toolchains[RUNTIME_TOOLS_TOOLCHAIN_TYPE]
    return toolchain.runtime_tools if toolchain else None

def program_runtime_tools(ctx, closure_names):
    """The tools this program gets, or None.

    Args:
        ctx: the rule context; the rule declares the optional runtime tools type.
        closure_names: the npm package names of the program's closure, or a
            function returning them, called only when the toolchain names
            `packages`.

    Returns:
        struct(tool_env = {name: FilesToRunProvider}, runfiles), or None.
    """
    tools = get_runtime_tools(ctx)
    if tools == None or not tools.tool_env:
        return None
    if tools.packages:
        wanted = {name: True for name in tools.packages}
        names = closure_names() if type(closure_names) == "function" else closure_names
        if not [name for name in names if name in wanted]:
            return None
    return struct(tool_env = tools.tool_env, runfiles = tools.runfiles)

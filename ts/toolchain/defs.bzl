"""The toolchain contract of rules_typescript.

Everything a rule outside this ruleset needs in order to run oxc, tsgo, the Go
tools, the launcher or a JavaScript runtime, and the rule declaring the tools a
launched program spawns: the toolchain type labels to
declare, the providers they carry, and the accessors that unwrap them.  The
implementations live in //ts/private, which by convention is loadable only
from within //ts.
"""

load(
    "//ts/private:runtime.bzl",
    _JS_RUNTIME_TOOLCHAIN_TYPE = "JS_RUNTIME_TOOLCHAIN_TYPE",
    _JS_TOOL_TOOLCHAIN_TYPE = "JS_TOOL_TOOLCHAIN_TYPE",
    _JsRuntimeInfo = "JsRuntimeInfo",
    _get_js_runtime = "get_js_runtime",
    _get_js_tool = "get_js_tool",
)
load(
    "//ts/private:runtime_tools.bzl",
    _RUNTIME_TOOLS_TOOLCHAIN_TYPE = "RUNTIME_TOOLS_TOOLCHAIN_TYPE",
    _RuntimeToolsInfo = "RuntimeToolsInfo",
    _get_runtime_tools = "get_runtime_tools",
    _ts_runtime_tools = "ts_runtime_tools",
)
load(
    "//ts/private:toolchain.bzl",
    _LAUNCHER_TOOLCHAIN_TYPE = "LAUNCHER_TOOLCHAIN_TYPE",
    _LauncherInfo = "LauncherInfo",
    _OXC_TOOLCHAIN_TYPE = "OXC_TOOLCHAIN_TYPE",
    _OxcToolchainInfo = "OxcToolchainInfo",
    _TOOLS_TOOLCHAIN_TYPE = "TOOLS_TOOLCHAIN_TYPE",
    _TSGO_TOOLCHAIN_TYPE = "TSGO_TOOLCHAIN_TYPE",
    _ToolsInfo = "ToolsInfo",
    _TsgoToolchainInfo = "TsgoToolchainInfo",
    _get_launcher_toolchain = "get_launcher_toolchain",
    _get_oxc_toolchain = "get_oxc_toolchain",
    _get_tools_toolchain = "get_tools_toolchain",
    _get_tsgo_toolchain = "get_tsgo_toolchain",
)

OXC_TOOLCHAIN_TYPE = _OXC_TOOLCHAIN_TYPE
TSGO_TOOLCHAIN_TYPE = _TSGO_TOOLCHAIN_TYPE
TOOLS_TOOLCHAIN_TYPE = _TOOLS_TOOLCHAIN_TYPE
LAUNCHER_TOOLCHAIN_TYPE = _LAUNCHER_TOOLCHAIN_TYPE
JS_RUNTIME_TOOLCHAIN_TYPE = _JS_RUNTIME_TOOLCHAIN_TYPE
JS_TOOL_TOOLCHAIN_TYPE = _JS_TOOL_TOOLCHAIN_TYPE
RUNTIME_TOOLS_TOOLCHAIN_TYPE = _RUNTIME_TOOLS_TOOLCHAIN_TYPE

OxcToolchainInfo = _OxcToolchainInfo
TsgoToolchainInfo = _TsgoToolchainInfo
ToolsInfo = _ToolsInfo
LauncherInfo = _LauncherInfo
JsRuntimeInfo = _JsRuntimeInfo
RuntimeToolsInfo = _RuntimeToolsInfo

get_oxc_toolchain = _get_oxc_toolchain
get_tsgo_toolchain = _get_tsgo_toolchain
get_tools_toolchain = _get_tools_toolchain
get_launcher_toolchain = _get_launcher_toolchain
get_js_runtime = _get_js_runtime
get_js_tool = _get_js_tool
get_runtime_tools = _get_runtime_tools

ts_runtime_tools = _ts_runtime_tools

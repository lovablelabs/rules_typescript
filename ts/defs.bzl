"""Public API for rules_typescript.

Users should load rules from this file:
    load("@rules_typescript//ts:defs.bzl", "ts_compile", "ts_test", "ts_binary")
    load("@rules_typescript//ts:defs.bzl", "TsInfo", "BundlerInfo")
    load("@rules_typescript//ts:defs.bzl", "DevServerInfo", "TsTestRunnerInfo")
    load("@rules_typescript//ts:defs.bzl", "ts_pnpm", "ts_add_package", "ts_refresh_tsconfig")
    load("@rules_typescript//ts:defs.bzl", "ts_codegen")
"""

load("//ts/private:pnpm.bzl", _ts_add_package = "ts_add_package", _ts_pnpm = "ts_pnpm")
load(
    "//ts/private:providers.bzl",
    _BundlerInfo = "BundlerInfo",
    _DevServerInfo = "DevServerInfo",
    _TsInfo = "TsInfo",
    _TsTestRunnerInfo = "TsTestRunnerInfo",
)
load("//ts/private:ts_binary.bzl", _ts_binary = "ts_binary_macro")
load("//ts/private:ts_codegen.bzl", _ts_codegen = "ts_codegen")
load("//ts/private:ts_config.bzl", _ts_config = "ts_config")
load("//ts/private:ts_dev_server.bzl", _ts_dev_server = "ts_dev_server")
load(
    "//ts/private:tsconfig_aspect.bzl",
    _ts_refresh_tsconfig = "ts_refresh_tsconfig",
)
load("//ts/private/actions:format.bzl", _ts_format = "ts_format")
load("//ts/private/rules:ts_compile.bzl", _ts_compile = "ts_compile")
load("//ts/private/rules:ts_test.bzl", _ts_test = "ts_test_runnable")

# Providers — exported for use in custom rules that extend this ruleset.
TsInfo = _TsInfo
BundlerInfo = _BundlerInfo
DevServerInfo = _DevServerInfo
TsTestRunnerInfo = _TsTestRunnerInfo

# The compile rule: srcs, deps, tsconfig. Every compiler option is the
# tsconfig's; the emit knobs are the flags in //ts:BUILD.bazel.
ts_compile = _ts_compile

# Standalone rules for advanced use cases.
ts_codegen = _ts_codegen
ts_config = _ts_config
ts_test = _ts_test
ts_format = _ts_format

# Runnable entry point; `bundler` takes any target returning BundlerInfo.
ts_binary = _ts_binary

# Dev server rule; `server` takes any target returning DevServerInfo.
ts_dev_server = _ts_dev_server

# Hermetic pnpm workspace macros, and the IDE tsconfig's run target.
ts_pnpm = _ts_pnpm
ts_add_package = _ts_add_package
ts_refresh_tsconfig = _ts_refresh_tsconfig

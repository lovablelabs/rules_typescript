### Added

- **A launched program gets native executables by environment variable,
  through the `//ts/toolchain:runtime_tools_type` toolchain.**
  `ts_runtime_tools(tool_env = {label: "NAME"}, packages = [...])`, exported
  from `//ts/toolchain:defs.bzl`, declares the executables, built for the
  target platform; `ts_test`, `ts_binary`, `ts_dev_server` and `npm_bin`
  resolve the type optionally, merge the tools' runfiles, and the launcher
  sets each variable to its tool's absolute path after `env`. A name `env`
  also sets is an analysis error. With `packages`, only a program whose npm
  closure holds one of the named packages, or that lies in a workspace member
of that name, gets the tools, so a tool change
  reruns only the programs that can spawn it. The ruleset registers no
  instance; [Runtime Tools](https://mikn.github.io/rules_typescript/guides/runtime-tools/)
  is the guide.

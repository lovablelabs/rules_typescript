### Added

- **`ts.check(binary, args)` names the type checker TsgoCheck runs.** Unset,
  it is the tsgo toolchain's compiler, as before. A configured checker is
  called with `args` and then tsgo's command line and has to print and exit
  as tsgo does, since the step parses its `--explainFiles` listing:
  `ts.check(binary = "//tools:oj", args = ["typecheck"])` checks on `oj typecheck`.
  Declaration emit, the editor project and Gazelle stay on tsgo.
  `--@rules_typescript//ts:check=@rules_typescript//ts:tsgo_check` runs one
  build on tsgo whatever the call names.

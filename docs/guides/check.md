# Type checker

Every `ts_compile` type-checks its program in a `TsgoCheck` validation
action, on the tsgo toolchain's compiler unless the root `MODULE.bazel` names
another checker:

```python
ts = use_extension("@rules_typescript//ts:extensions.bzl", "ts")
ts.check(
    binary = "//tools:oj",
    args = ["tsc"],
)
```

| Attribute | Type | Default | Description |
|-----------|------|---------|-------------|
| `binary` | `label` | required | The checker's executable target. A source file needs a rule around it (`copy_file` with `is_executable = True`) |
| `args` | `string_list` | `[]` | Arguments passed before tsgo's command line: `["tsc"]` for `oj tsc` |

The checker is called as `binary args... --project <tsconfig> --noEmit
--explainFiles --pretty false`, plus `--checkers N` under
`--//ts:checkers=N`, from the program root tsgo would run in. It has to print
and exit as tsgo does: the step relays its diagnostics when it fails and
checks the `--explainFiles` listing against the target's deps when it passes
([Deps Have to Be Direct](../rules/ts-compile.md)). `oj tsc` is such a
checker.

Only `TsgoCheck` changes. Declaration emit (`TsgoDeclare`), the editor
project (`TsIdeProject`), the config the actions read (`tsgo --showConfig`)
and Gazelle's listing stay on tsgo, so the tsgo toolchain is still required.

Only the root module's call takes effect. One build on tsgo whatever the call
names: `--@rules_typescript//ts:check=@rules_typescript//ts:tsgo_check`.

# Lint

The linter is configured once, in the root `MODULE.bazel`, and every
`ts_compile` and `ts_test` runs it over its own sources as a validation
action -- the shape of `nogo` in rules_go.

```python
ts = use_extension("@rules_typescript//ts:extensions.bzl", "ts")
ts.lint(
    binary = "@npm//:oxlint_bin",
    config = "//:oxlint.json",
)
```

| Attribute | Type | Default | Description |
|-----------|------|---------|-------------|
| `binary` | `label` | required | The linter's executable from the workspace's hub: `@npm//:oxlint_bin` or `@npm//:eslint_bin`. The lockfile decides which oxlint or eslint runs; there is no linter toolchain |
| `config` | `label` | `None` | The linter's own config file (`oxlint.json`, `.oxlintrc.json`, `eslint.config.mjs`, ...), passed as `--config`. Unset, no flag is passed: oxlint runs its defaults, eslint needs a flat config |
| `data` | `label_list` | `[]` | Config imports, local plugins, ignore files and `node_modules` targets used by the config. Importer directories come from those targets and the program |
| `args` | `string_list` | `[]` | Additional CLI arguments. `{tsconfig}` expands to the generated target-specific compiler config |
| `tool_env` | `label_keyed_string_dict` | `{}` | Executable labels mapped to environment variable names. Values passed to the linter are absolute executable paths; tools and their runfiles are action inputs |
| `fail_on_warnings` | `bool` | `False` | A warning fails the build: `--max-warnings=0`, which oxlint and eslint spell alike |

Only the root module's call takes effect, and it makes one; a module graph
with no call lints nothing. The config file is read from another repository,
so its package exports it: `exports_files(["oxlint.json"])`. One build without
the linter: `--@rules_typescript//ts:lint=@rules_typescript//ts:no_lint`, a
`lint_config` naming no binary.

## What Runs

The call writes the `@lint_config` repository, whose one target the label flag
`@rules_typescript//ts:lint` names. Every `ts_compile` and `ts_test` reads the
flag and, when it names a binary, runs one `TsLint` action over the target's
TypeScript, JavaScript and declaration srcs, never a data src, and writes
`<name>.tslint` when the linter exits 0. The stamp is in the
`_validation` output group, the position of tsgo's own check:

- it runs during `bazel build`, without being asked for;
- it does not block downstream compilation: a lint error in a library does not
  stop a binary that depends on it from being built;
- it is cached against the declared source, config, plugin and program inputs.

To run the checks alone, or where a `.bazelrc` turned validations off:

```bash
bazel build //... --output_groups=+_validation
```

## Paths

TsLint runs from the same workspace-shaped program layout as tsgo, with the target's compiler config, its config chain, dependency type inputs and npm files. Sources and config paths stay workspace-relative, preserving config override and ignore patterns. npm binary launchers preserve their caller's working directory.

Declare imported config modules, local plugins (including their dynamic imports), ignore files and any package manifests they need in `data`. Source config/data files are copied into the program layout because Node resolves imports from a file's real path; a symlink would send those imports outside the staged importer tree. Generated config/data remains at its output-tree path: it is not copied back to a source-package path. Generated JS/TS configs and plugins therefore do not get the source-file guarantee for relative imports, npm resolution or config-relative overrides. Use source configs/plugins when relying on that behavior.

A config importing npm packages also needs its `node_modules` target in `data`, even when the target being linted has no npm dependencies. This supplies the config's package files and importer layout without adding lint-only packages to the TypeScript dependency graph.

The program layout exposes the generated compiler config through a `tsconfig.json` reference at the target package and any declared nested config locations. Linters that discover the nearest config therefore see the same target program as native checking. The reference sets `noEmit` for lint validation, so JavaScript programs do not request output over their inputs. The canonical config retains the native compiler's resolved options, including inherited type roots, so discovery does not reenter the original extends chain. Original source configs stay unchanged.

For oxlint type-aware checks, use `args = ["--type-aware"]` and declare the importer holding the config’s npm dependencies in `data`. Set `tool_env = {"@npm//:oxlint-tsgolint_bin": "OXLINT_TSGOLINT_PATH"}` to provide the type-aware executable; package files in `data` do not create npm `.bin` launchers. The binding takes any executable that answers oxlint's backend call, `$OXLINT_TSGOLINT_PATH headless` with the payload on stdin. oj 0.4.12 and newer does (the backend behind `oj lint`: tsgolint's rules on the checker `oj typecheck` runs), so a workspace that already names an oj binary in `ts.check` can bind it here, `tool_env = {"//tools:oj": "OXLINT_TSGOLINT_PATH"}`, and lint without the `oxlint-tsgolint` package. Its rules follow a newer tsgolint than an old `oxlint-tsgolint` pin, so expect reports the old pin did not make. Other CI flags, such as `--report-unused-disable-directives-severity=error`, belong in the same `args` list. The selected linter version must support the flags.

## Action Environment

The action starts with `PATH=/bin:/usr/bin` and no inherited host environment. Each `tool_env` binding supplies the declared executable’s absolute execroot path before entering the program layout; it does not read a host variable or invoke a shell. Node comes from the wrapper's own runfiles, staged because the
linter’s and auxiliary executables’ `FilesToRunProvider`s are passed in `tools`. A linter
binary whose native sidecar or Node cannot be found is usually a non-executable
target.

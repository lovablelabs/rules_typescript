# Runtime Tools

A native executable a JavaScript program spawns at run time -- a compiler
server, a formatter, a code generator a Vite plugin calls -- reaches every
launched program through one toolchain: `ts_runtime_tools` names the
executables and the environment variable each is handed by, and `ts_test`,
`ts_binary`, `ts_dev_server` and `npm_bin` resolve it. The launcher sets each
variable to its executable's absolute path, so the program reads a path it can
spawn, under `bazel test` and `bazel run` alike.

```python
load("@rules_typescript//ts/toolchain:defs.bzl", "ts_runtime_tools")

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
```

```python
# MODULE.bazel
register_toolchains("//tools/compiler:compiler_toolchain")
```

| Attribute | Type | Default | Description |
|-----------|------|---------|-------------|
| `tool_env` | `label_keyed_string_dict` | required | Executable labels mapped to environment variable names. Built for the target platform: the tool runs where the program runs |
| `packages` | `string_list` | `[]` | npm package names. Set, only a program whose npm closure holds one of them gets the tools; the others carry neither the variables nor the tools' files, so a change to a tool reruns only the tests that can spawn it |

The rules declare the type optional: with nothing registered, programs run as
before. One toolchain resolves per target, so one `ts_runtime_tools` lists
every tool a workspace hands out.

## What a Program Gets

- Each tool's executable and runfiles join the program's runfiles.
- The launcher config's `tool_env` names each tool by its runfiles path; the
  launcher resolves it after `env` and before the program starts, so the value
  is absolute whatever directory the program runs from (a dev server runs from
  its package in the source tree).
- A name the target's `env` also sets is an analysis error naming the tool; a
  `tool_env` name must be a valid environment variable name.

`TS_LAUNCHER_DUMP_CONFIG=1 bazel run //target` prints the resolved values.

## Which Programs Match `packages`

| Rule | The npm closure `packages` is matched against |
|------|-----------------------------------------------|
| `ts_test` | The program's npm closure, the one its runner's packages are checked against |
| `ts_binary` | The entry's and `data`'s npm closures and every package the `node_modules` chain links |
| `ts_dev_server` | The entry's npm closure, the `node_modules` chain and the workspace members the server binds |
| `npm_bin` | The package itself and the dependencies its store links |

For `ts_test`, `ts_binary` and `ts_dev_server` the workspace member the
target's own package lies in counts too, found by its directory among the
members of the lockfile the `node_modules` chain belongs to, so a member's
test matches its own name when it reaches the member through the compiling
target. The match is by package name, workspace members included. A program reaching
the tool only through a package outside this closure -- one it loads by path
-- gets no tools; name that package in `packages` too, or leave `packages`
empty.

## Platforms

The toolchain is constrained like any other: a tool that builds for some
target platforms only takes `target_compatible_with` on its `toolchain()`, and
a program for any other platform resolves none and runs without the variables.

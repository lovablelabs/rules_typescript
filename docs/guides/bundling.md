# Bundling

`ts_binary` collects the transitive `.js` outputs of its entry point and either
runs the entry directly or, given a `bundler`, hands the graph to a bundler and
runs the bundle. The ruleset ships no bundler; one plugs in through
[`BundlerInfo`](#custom-bundler-bundlerinfo-interface).

A Cloudflare Worker is bundled by wrangler, outside the build. See
[Testing § Cloudflare Workers](testing.md#cloudflare-workers) for running its
tests inside workerd.

## Running Without a Bundler

`ts_binary` with no `bundler` runs the entry point's own `.js` on the JS runtime,
with the transitive `.js` in its runfiles. The imports resolve as written;
nothing is bundled or concatenated.

```python
load("@rules_typescript//ts:defs.bzl", "ts_binary")

ts_binary(
    name = "app",
    entry_point = "//src/app",
)
```

```bash
bazel run //:app
```

## Custom Bundler (BundlerInfo Interface)

Any Bazel rule that returns `BundlerInfo` plugs into `ts_binary`;
`rules_typescript` itself needs no change for a bundler.

```python
load("@rules_typescript//ts:defs.bzl", "BundlerInfo")

def _my_bundler_impl(ctx):
    return [BundlerInfo(
        bundler_binary = ctx.attr.binary[DefaultInfo].files_to_run,
        config_file = None,                 # optional static config
        runtime_deps = depset([]),           # files needed at bundle time
        use_generated_config = False,        # set True for a generated Vite config
    )]

my_bundler = rule(
    implementation = _my_bundler_impl,
    attrs = {
        "binary": attr.label(
            executable = True,
            cfg = "exec",
        ),
    },
)
```

```python
ts_binary(
    name = "app",
    entry_point = "//src/app",
    bundler = ":bundler",
    format = "esm",
    sourcemap = True,
    external = ["react", "react-dom"],
)
```

Pass the executable target's `DefaultInfo.files_to_run` so Bazel stages its runfiles with the tool. A standalone executable without runfiles may use a `File`; `runtime_deps` supplies additional action inputs.

Bundling uses the same [entry selection](../rules/ts-binary.md#without-a-bundler)
as direct execution, including `entry_file` for targets with multiple outputs.

A `.css`, a `*.module.css`, an image or any other data src in a
`ts_compile`'s `srcs` reaches the bundler through its
`TsInfo.transitive_data`, so every non-JS file the graph imports is in the
sandbox beside the compiled `.js` that imports it.

### BundlerInfo Invocation Modes

**Mode 1: Standard CLI** (`use_generated_config = False`, the default)

`ts_binary` invokes the bundler binary with:

```
<bundler_binary>
  --entry  <path/to/entry.js>
  --out-dir <output/dir>
  --format esm|cjs|iife
  [--external <pkg>]...
  [--sourcemap]
  [--define <key>=<value>]...
  [--config <config_file>]   (only when config_file is set)
```

Output is expected at `<out-dir>/<bundle_name>.js` (and `.js.map` if `--sourcemap`).

**Mode 2: Generated Config** (`use_generated_config = True`)

`ts_binary` generates a Vite lib-mode `vite.config.mjs` carrying the format,
externals, `define` map, source-map setting and a `resolve.alias` entry per
compiled `.js`, and invokes the binary with four positional arguments, all
execroot-relative:

```
<bundler_binary> \
  <generated vite.config.mjs> \
  <entry .js> \
  <output dir> \
  <stylesheet>
```

The config reads the entry and the output directory back from `VITE_ENTRY_PATH`
and `VITE_OUT_DIR` and rebuilds every alias path from `EXEC_ROOT`, so the binary
sets those three (`EXEC_ROOT` to the directory it was started in) and runs
`vite build --config` on the config, with Vite resolved from its own
`runtime_deps`. It creates the stylesheet before the build: lib mode extracts
every imported stylesheet into it and never references it from the JS, so only
the declaration keeps the file, and an entry importing no CSS still has to
produce it. The declared outputs, inside `<name>_bundle/`:

| Format | Output file |
|--------|-------------|
| `esm` | `<bundle_name>.es.js` |
| `cjs` | `<bundle_name>.cjs.js` |
| `iife` | `<bundle_name>.iife.js` |

plus `<that>.map` under `sourcemap = True`, and `<bundle_name>.css`, the
stylesheet lib mode extracts; a `*.module.css` is scoped by Vite's own CSS
modules.

### BundlerInfo Fields

| Field | Type | Description |
|-------|------|-------------|
| `bundler_binary` | `File` or `FilesToRunProvider` | Standalone executable, or executable target with its runfiles |
| `config_file` | `File` or `None` | Optional static config passed via `--config` (mode 1 only) |
| `runtime_deps` | `depset of File` | Files the bundler needs at runtime |
| `use_generated_config` | `bool` | When `True`, use mode 2 (generated vite.config.mjs) |

## Attributes

| Attribute | Type | Default | Description |
|-----------|------|---------|-------------|
| `entry_point` | `label` | required | `ts_compile` target providing `TsInfo` |
| `bundler` | `label` | `None` | Target providing `BundlerInfo` |
| `bundle_name` | `string` | rule name | Output file name (without `.js`) |
| `format` | `string` | `"esm"` | Output format: `esm`, `cjs`, `iife` |
| `sourcemap` | `bool` | `True` | Emit source map |
| `external` | `string_list` | `[]` | Module specifiers to leave external |
| `define` | `string_dict` | `{}` | Global constant replacements |

`entry_file` (which `.js` is the entry when the target emits several), `data`
and `node_modules` are the rest of the rule. See the
[ts_binary reference](../rules/ts-binary.md#attributes).

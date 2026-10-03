# ts_binary

Produces a runnable target from a `ts_compile` entry point, or from a plain
JavaScript file. Without a bundler it runs that entry `.js` on the JS runtime;
with one it bundles first and runs the bundle.

## Usage

```python
load("@rules_typescript//ts:defs.bzl", "ts_binary")

ts_binary(
    name = "app",
    entry_point = "//src/app",
    format = "esm",
    sourcemap = True,
)
```

## Attributes

| Attribute | Type | Default | Description |
|-----------|------|---------|-------------|
| `entry_point` | `label` | required | `ts_compile` target providing `TsInfo`, or a single `.js`/`.mjs`/`.cjs` source file |
| `entry_file` | `string` | `""` | Source filename selecting one output, e.g. `"main.ts"`; see default selection below |
| `data` | `label_list` | `[]` | Extra runfiles: sibling modules a source `entry_point` imports, fixtures, anything read at runtime. `TsInfo` targets retain their declared runtime closure and npm bindings, including with a bundled entry |
| `bundler` | `label` | `None` | Target providing `BundlerInfo`. When set, the bundle is what runs |
| `bundle_name` | `string` | rule name | Output file name (without `.js`) |
| `format` | `string` | `"esm"` | Output format: `esm`, `cjs`, `iife` |
| `sourcemap` | `bool` | `True` | Emit source map |
| `external` | `string_list` | `[]` | Module specifiers to leave external |
| `define` | `string_dict` | `{}` | Global constant replacements |
| `node_modules` | `label` | `None` | The importer's [`node_modules`](node-modules.md) target: its links and store trees are runfiles and the directory is on `NODE_PATH` |

## A JavaScript File as the Entry Point

`entry_point` also takes a `.js`, `.mjs` or `.cjs` source directly, for a
program that is already plain JavaScript, such as a
[`ts_codegen`](ts-codegen.md) generator:

```python
ts_binary(
    name = "gen_schema",
    entry_point = "generate-schema.mjs",
    data = ["schema-helpers.mjs"],
    node_modules = "//:node_modules",
)
```

The modules the entry imports go in `data`. A build action materializes declared JavaScript modules at their admitted runfiles coordinates, with ordinary data and npm stores copied once per File authority and linked into that layout. The launcher execs the configured runtime and preserves the caller's working directory. It does not remove the view when the original process exits. Directory and manifest-only launches use the same built view. The native child's standard runfiles lookup uses this same view and Bazel's generated repository mapping, so loading a module through runfiles does not create a second instance. Package the launcher's runfiles with its config and complete runtime directory, preserving relative links. See [runtime input lifetime](providers.md#runtime-input-lifetime).

A `.ts` entry point is refused with a message pointing at `ts_compile`; this
rule does not compile TypeScript.

## Without a Bundler

Without a `bundler`, `ts_binary` runs the entry point's own `.js` file on the JS
runtime, with the transitive `.js` outputs and imported JSON at their declared
runfiles paths. The imports resolve as written; staging adds no missing output
aliases or source transforms.

Without `entry_file`, a sole output is the entry. For several outputs, the
binary selects the sole source from the compiler's own package, or its unique
local `index.js` when there are several local sources. Retaining a foreign
`index.ts` as an imported helper cannot replace that local entry. Set
`entry_file` when the local choice is ambiguous. If several outputs share that
filename, use an entry target with one entry output. Older custom providers without
source-to-output pairs retain the single-output or unique-`index.js` convention.

Native binaries preserve every positive npm binding declared by each source owner. The build action places links to the selected source store authority beside the admitted runtime module. It also preserves the source package scope's bindings for external `#imports` targets, which Node resolves from that scope. A nested module and its enclosing scope can therefore retain different versions of the same npm name. A free projection destination receives a link to that authority; an occupied destination must already resolve to it. Aliases share package identity within the view.

Placement fails before application execution when a different entry occupies the destination, an opaque ancestor blocks it, or it would hide declared descendants. The active workspace-root `node_modules` alias can supply the same store, but placement cannot replace it and discard its other packages. Declared bindings remain even when the compiler erases their imports, so an occupied destination can prevent launch without a corresponding runtime request. Relocate the conflicting entry or keep incompatible contexts in separate runtime directories.

Node selects package-import conditions and patterns and resolves package exports. Older providers without binding facts and custom bundlers retain their own contracts.

Use `tsaction` and the launcher built from the same ruleset revision. The normal toolchains build both from source. An older `tsaction` rejects the native-view command, and an older custom launcher rejects the new config field before execution. Optional provider fields let new consumers read older records; they do not make old tools understand the new action or config.

## With a Bundler

Any rule returning `BundlerInfo` plugs in; the ruleset ships none. See [Bundling § Custom Bundler](../guides/bundling.md#custom-bundler-bundlerinfo-interface).

# Gazelle Directives Reference

Ordinary TypeScript program membership and dependencies come from the compiler listing, lockfile and manifest ([Overview](overview.md)). Protobuf generation additionally needs explicit product identities: a schema can produce different plugin options and output roots for different consumers.

## Protobuf generation identities

At a common schema ancestor, declare a `ts_proto_config` target. Its name identifies the product; `out_dir` is relative to that BUILD package. `tsconfig`, optional `node_modules` and `deps` are Bazel labels. Options default to `target=ts`. Gazelle reads literal strings and string lists from the existing rule; expressions such as `select()` are rejected. Overlapping output roots are rejected.

```python
load("@rules_typescript//proto:defs.bzl", "ts_proto_config")

# gazelle:ts_proto json

ts_proto_config(
    name = "json",
    out_dir = "generated/json",
    tsconfig = "//settings:tsconfig",
    deps = ["@npm//:bufbuild_protobuf"],
    options = ["target=ts", "json_types=true"],
)
```

`ts_proto_config` defines an identity without selecting it; generation defaults off. `# gazelle:ts_proto` selects space-separated identity names for the current directory and descendants; `none` disables generation there. Native proto rules still own schema membership and canonical import names. This configuration adds no schema globs or protobuf namespace filters. Declare multiple identities when one schema needs different outputs.

Generated wrappers inherit ambient type dependencies from the compiler program listed for the selected `ts_config` source (or directly selected `tsconfig.json`). Custom configuration files without an observed program retain their explicit `deps`; Gazelle does not infer types from another configuration in the wrapper's directory.

Wrappers are generated in the identity's ancestor package, with target names `<identity>/<native-package-relative-path>/<native-target>`. All wrappers for one identity share its output root, preserving relative generated imports across native packages. The compiler configuration and runtime labels are relative to the ancestor package. Generated sibling dependencies resolve only within the same identity. Runtime-provided well-known imports come from the pinned protobuf runtime's metadata. Every other imported schema needs a generated wrapper in the same owner and output identity. Adding an external proto target to `deps` does not adopt its schemas; declare its native graph as described below.

Run ordinary Gazelle recursively on the configured ancestor or the repository root. A child-only or nonrecursive update affecting that owner fails before BUILD writes and gives the complete rerun command. Excluded or disabled directories do not contribute wrappers. Compose a Gazelle binary with the native proto language before TypeScript. The default standalone TypeScript binary retains its TypeScript-only scope.

Generated protobuf target names use the reserved `<identity>/<relative proto package>/<native target>` shape. Removing an identity removes its targets on the next complete owner update; ordinary target names and `# keep` remain outside that transition.

### External native graphs

`proto_graph` from `@rules_typescript//gazelle:proto_graph.bzl` takes native root labels in `deps`. It derives canonical source paths and configured native dependency edges through an aspect, under the same protobuf source-info configuration as generation. The artifact contains no generation options or output roots; identities retain those facts.

Add the graph target to the custom `gazelle` runner's `data` and pass `-ts_proto_graph=$(rlocationpath :external_graph)` through `args`. The [integration fixture](https://github.com/mikn/rules_typescript/blob/c8be058bf72d19612a1d622d3da3cbc2df6f8507/tests/integration/proto_generated/BUILD.bazel) shows this attachment. Declare roots only in the graph rule. Available schemas are emitted only when a selected local identity reaches them; unrelated available roots stay absent.

External dependencies retain their native labels. A local import override cannot rewrite an external target's configured dependency. Declared-root spellings map to their Bazel-resolved canonical identities; transitive targets use canonical labels. An apparent-name override for a target that is not a declared root fails with an actionable diagnostic; use its canonical label.

External wrappers use `<identity>/_external/<canonical repository>/<native package>/<native target>`. Empty forwarding targets and aliases lead to their direct source owners without duplicating wrappers. Removing the importing selection removes its unused external wrappers on the next complete update. The graph is a rebuildable Bazel output, never a manually maintained schema inventory.

## `# keep`

`# keep` is Gazelle's own comment, understood by every language extension. Above
an attribute it means "never rewrite this value"; above a whole rule, "never
rewrite this rule". Use it when a run keeps undoing an edit:

```python
ts_compile(
    name = "internal",
    srcs = ["index.ts"],
    # keep
    visibility = ["//myapp:__subpackages__"],
)
```

`visibility` is the common case: it is a merged attribute and every rule Gazelle
generates carries `//visibility:public`, so without `# keep` a hand-narrowed
visibility widens back on every run that re-emits the rule. See
[getting the clean-tree diff to empty](overview.md#getting-the-clean-tree-diff-to-empty).

### Attributes Gazelle Owns

Gazelle recomputes these from the tree on every run, so a value it cannot derive
is replaced unless a `# keep` holds it:

| Rule | Attributes Gazelle owns |
|------|-------------------------|
| `ts_proto_library` | `proto`, `out_dir`, `tsconfig`, `node_modules`, `options`, `deps`, `type_inputs`, `visibility` |
| `ts_compile` | `emit`, `srcs`, `package_scopes`, `type_inputs`, `deps`, `tsconfig`, `visibility` |
| `ts_test` | `emit`, `srcs`, `package_scopes`, `type_inputs`, `test_srcs`, `deps`, `tsconfig`, `config`, `config_srcs`, `config_node_modules`, `workers_pool`, `wrangler_config`, `coverage_provider` |
| `ts_config` | `src`, `deps`, `visibility` |
| `filegroup(name = "vitest_config")` | `srcs`, `visibility` |
| `filegroup(name = "wrangler_config")` | `srcs`, `visibility` |

`emit` defaults to `False`. Gazelle writes `True` when a Node binary/test or package manifest requires built outputs, and propagates that requirement through resolved dependencies. A custom output consumer needs an explicit `emit = True` with `# keep`. Removing the built-output consumer removes its generated opt-in on the next run.

`ts_config.deps` is the `extends` chain, a dep on the `ts_config` of every
`tsconfig.json` the file extends by a relative path. A base of another name
gets no label, and for an owned attribute that means the value goes: a
hand-written `deps` needs a `# keep` on its line to survive the next run. See
[the tsconfig and its ts_config](overview.md#the-tsconfig-and-its-ts_config).

!!! note "Upgrading"
    `ts_config.deps` used to be write-once; it is Gazelle's now. On every
    `ts_config` whose `deps` you wrote by hand, put `# keep` on the entry's own
    line to hold that entry beside what Gazelle computes, or above the
    attribute to keep the whole value. Without one, `deps` is recomputed entry
    by entry and each dropped entry is named in the log.

A tsconfig's `paths` and `types` are the rule's to read, so no attribute
restates them. What a path-shaped `types` entry asks of Gazelle is the dep on
the target that stages the file it names, written into `deps` like any other;
see [a declaration the tsconfig names](overview.md#a-declaration-the-tsconfig-names).

`ts_codegen`, `ts_dev_server`, `ts_pnpm` and `ts_add_package` are outside all
of this: Gazelle writes none of them and touches none, so each comes through
every run as written, load symbol included, `# keep` or not. A `ts_codegen` is
read, for its `outs` and `out_dir`, and never rewritten.

A directory that is not a package has every rule Gazelle would write there
withdrawn, under the names it would use; `# keep` above such a rule holds it.

`# keep` works at three granularities: one value, one attribute, one rule:

```python
ts_compile(
    name = "app",
    srcs = [
        "main.ts",
        "legacy.js",  # keep
    ],
    # keep
    tsconfig = "//:tsconfig_build",
)
```

A run that drops a value from one of these attributes reports it:

```
typescript: ts_compile(app) in BUILD.bazel: Gazelle generates srcs and
recomputed it from the tree, so "legacy.js" is no longer declared. A
value Gazelle cannot derive needs a "# keep" comment on its own line to survive
the next run; "# keep" above the attribute hands the whole attribute back to you.
```

Every dropped value is reported, whether it was a stale label Gazelle wrote
itself or an edit of yours. `deps` is the exception: it is filled in after
resolution, when the value on disk is no longer in hand, so a label you wrote
into it goes without a report unless `# keep` holds it.
Gazelle's Go extension drops the same values silently; what survives a run is
identical either way.

One case is silent: a value whose file is no longer on disk. Deleting a source
drops the entry that named it, and holding that entry with `# keep` would name a
source nothing provides, which fails analysis. A value whose file is still on
disk is always reported.

#### Values Gazelle Cannot Merge

For compiler `srcs`, automatic dependency discovery follows direct file labels, literal native `filegroup.srcs` and `alias.actual` chains, and declared output names. This also applies to extra kept roots in manifest-owned programs. Gazelle preserves the source spelling. Unknown membership (`glob`, `select`, opaque providers, non-default output groups or unobserved external targets) is not an empty source list: automatic closure stops without writing BUILD files. Use explicit source-file labels, or keep the whole rule or ignore its package and maintain both sources and deps. Literal empty filegroups remain valid.

`# keep` decides what survives among plain strings and plain lists of plain
strings, the two shapes Gazelle's merger reconciles value by value. A value in
any other shape it cannot merge at all: a module-level variable, two lists joined
with `+`, a `select()`, or a list with one variable element.

What happens then is the merger's decision, and it goes two ways by shape. A
bare variable is replaced with the value Gazelle derived, and whatever the
variable held is gone. Two lists joined with `+` are refused: the attribute keeps
your expression and Gazelle stops recomputing it. `rules_go`'s extension behaves
the same way, since both call the same merger. Neither outcome is silent here:

```
typescript: BUILD.bazel:28: ts_compile(app) declares srcs as an
expression Gazelle's merger cannot reconcile value by value, so srcs is
no longer an attribute Gazelle maintains: it either replaces the whole
expression, losing what it computed, or leaves it untouched and stops updating
it. A "# keep" comment above the attribute makes that yours deliberately.
```

Either way the attribute has stopped being maintained. Two resolutions: put
`# keep` above the attribute and own it, or rewrite the value as a plain list of
strings with `# keep` on the entries Gazelle cannot derive, which hands the
attribute back to it. For compiler `srcs`, the automatic-discovery boundary above still applies.

## `# gazelle:exclude`

Core Gazelle's, and the one way to keep Gazelle out of a tree.
`# gazelle:exclude <path>` in a BUILD file prunes the walk below that path, so
no BUILD file is written there and nothing under it is visited; `.bazelignore`
does the same for Bazel and Gazelle alike, and `# gazelle:ignore` in a
directory's own BUILD file leaves that file untouched while allowing imports
from it. An imported file under a pruned directory stops generation unless
a declared generator owns it. What a package compiles is its tsconfig's
`include`, `files` and `exclude`; what Bazel must not enter -- `node_modules`,
a generated directory, a fixture workspace with a `MODULE.bazel` of its own --
is excluded here.

```python
# BUILD.bazel (repo root)
# gazelle:exclude examples
```

## `# gazelle:resolve`

Core Gazelle's override of one resolution:
`# gazelle:resolve typescript <repository path> <label>`. The import string is
the repository path of the file tsgo listed as the edge's target, not the
specifier a source wrote, because `deps` is resolved by the file; the label is
what every edge to that file becomes, ahead of the rule whose `srcs` hold it.
It is the escape hatch for a first-party file no generated rule owns: a
declaration in a manually maintained package, or a file a hand-written rule
stages under another name. It does not bypass file exclusions; an excluded
file still needs a declared generator. For a raw generated TypeScript output,
the override can name a compiler whose `srcs` reaches the output through a
filegroup. Use the declared output's repository path; the same override applies
when the compiler probes an absent output and when its checkout copy is
excluded. A skipped missing directory has no scalar file probes; see
[generated-output resolution](../rules/ts-codegen.md).
An override of the compiler-resolved file also takes priority over a generated
tree candidate. Traversal stops at a selected generated owner for ordinary and
config imports, including when the compiler lists a lower-priority checkout fallback.
The directive is inherited by the
directories below the BUILD file that carries it, as every core directive is.

```python
# BUILD.bazel (repo root)
# gazelle:resolve typescript vendor/legacy/index.d.ts //vendor/legacy
```

## Removed Directives

!!! note "Upgrading"
    Remove the directives listed below. Core Gazelle reports unknown directives
    and continues; the table identifies each replacement.

| Directive | What does its job |
|-----------|-------------------|
| `# gazelle:ts_package_boundary` | the `tsconfig.json`: a directory is a package when its program lists a first-party file, and everything the program lists belongs to it |
| `# gazelle:ts_exclude` | the tsconfig's `include`, `files` and `exclude`; a tree Bazel must not enter is `# gazelle:exclude` or `.bazelignore` |
| `# gazelle:ts_exclude_dir` | `# gazelle:exclude` |
| `# gazelle:ts_ignore` | `# gazelle:ignore`, or no `tsconfig.json` |
| `# gazelle:ts_target_name` | a hand-written rule under the name wanted; Gazelle writes and withdraws only the names it would generate |
| `# gazelle:ts_js_srcs` | `allowJs` in the tsconfig: tsgo lists the `.mjs`, and it is a src |
| `# gazelle:ts_ambient_types` | `compilerOptions.types`, or a `/// <reference types>` directive; both are edges in the listing |
| `# gazelle:ts_runtime_dep` | the package's `package.json`: a `ts_test`'s deps carry its `dependencies` and `devDependencies` |
| `# gazelle:ts_warn_unresolved` | tsgo prints no line for a specifier it could not resolve, so the type check's `TS2307` is the report; a project the lockfile has no importer for, where every bare import would be one, gets no target |
| `# gazelle:ts_declarations` | the flag `--//ts:declarations`, one value for the whole build |
| `# gazelle:ts_path_alias` | `compilerOptions.paths`, read by tsgo when it lists the program and by the rule when it checks it |
| `# gazelle:ts_codegen` | a hand-written `ts_codegen`; Gazelle indexes its `outs` and `out_dir` and never writes one |
| `# gazelle:ts_npm_hub` | every npm label Gazelle writes names `@npm`, the hub of the root lockfile it reads; a package on another hub writes its deps by hand under `# keep` |
| `# gazelle:ts_npm_mapping` | the lockfile is the inventory; a package from a label of your own is a `# keep` dep |
| `# gazelle:ts_asset_declaration_type` | the tsconfig or a declaration file in `srcs`: `vite/client` in `types`, or a `declare module "*.svg"` |

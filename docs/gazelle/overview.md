# Gazelle Overview

Gazelle writes the BUILD files: one package per compiler program, its
targets' sources and deps read off tsgo's own listing of that program.

## Setup

Add the Gazelle binary to your root `BUILD.bazel`:

```python
load("@gazelle//:def.bzl", "gazelle")

gazelle(
    name = "gazelle",
    gazelle = "@rules_typescript//gazelle:gazelle_typescript",
    tags = ["manual"],
)
```

`gazelle_typescript` carries the TypeScript language alone. The other binary in
that package, `gazelle_ts`, also carries Go and proto; rules_typescript uses it
to generate BUILD files for its own `.go` sources. In a polyglot repo it
rewrites Go BUILD files too.

Add `gazelle` to `MODULE.bazel`:

```python
bazel_dep(name = "gazelle", version = "0.47.0")
```

`rules_typescript` declares `rules_go`, `go_sdk` and `go_deps` as non-dev
dependencies, so they propagate transitively via bzlmod. Consumers need only the
`bazel_dep` above, and no Go toolchain of their own. Gazelle compiles only
under `bazel run`: `tags = ["manual"]` keeps the target and its runner out of
`bazel build //...`, and the run fetches a Go SDK and its modules. `bazel build`
and `bazel test` compile no Go unless the workspace registers
[tsgo from source](../rules/providers.md#tsgo-from-source); the ruleset's
other Go tools are the binaries of its
[tools release](../RELEASE_PROCESS.md#tools).

Run Gazelle:

```bash
bazel run //:gazelle
```

## What a Run Reads

Five things, without program-membership directives.

1. **Every `tsconfig.json`**, listed through tsgo from the repository root:
   `tsgo -p <dir>/tsconfig.json --noEmit --listFilesOnly --explainFiles
   --pretty false`. The listing is the program's files and every edge between
   them -- each import, module augmentation, `/// <reference>` directive and
   `types` entry, with the file it resolved to. The binary is the toolchain's,
   carried in `gazelle_typescript`'s runfiles, so the program Gazelle reads is
   the one the build type-checks; `-ts_tsgo=<path>` names another. Two files
   are never listed: a root `tsconfig.json` whose extends chain sets neither
   `include` nor `files`, since tsgo would enumerate the whole repository, and
   a `tsconfig.json` `ts_refresh_tsconfig` wrote, built out of the very targets
   that would name it.
2. **`pnpm-lock.yaml`** at the repository root, read once: every name the hub
   declares, the importers and what each declares, and the `link:` entries
   that make a directory a workspace member.
3. **The nearest `package.json`** above a package: its `name`, and for a
   `ts_test` its `dependencies` and `devDependencies`. Without a local
   tsconfig, existing JavaScript and TypeScript entry points from `exports`,
   `types`, `typings`, `main`, `module` and `browser` seed the same native
   compiler listing with JavaScript enabled.
4. **The vitest configs** the generated tests name, listed during generation
   while Gazelle's native walk can check exclusions for their imports. Each
   selected config is listed on its first request; tests using the same config
   share its observed program. Its imports and their source closure supply the
   test's runtime.
5. **The hand-written `ts_codegen` rules** in the BUILD files walked: their
   `outs` and every `out_dir`, the target's output whatever a local run of the
   generator left on disk.

The listing resolves through the checkout's `node_modules`, so a repository
with a root lockfile is installed before a run. tsgo prints no line for an
import a missing install leaves unresolved, and a root `pnpm-lock.yaml` with no
`node_modules/.modules.yaml` beside it stops the run before any `deps` is
written. A `tsconfig.json` whose nearest `package.json` is no importer in the
lockfile is not listed at all: pnpm installs nothing for that project, so it is
foreign to the workspace ([The Package Model](#the-package-model)).

A run says nothing else about the listings, tsgo's diagnostics included.
`-ts_verbose` prints which binary ran, one line per `tsconfig.json` with what
it listed or why it was not, its diagnostics, how many are packages, and the
`.ts`/`.tsx`/`.mts`/`.cts` files no program lists, per directory and in total:
`bazel run //:gazelle -- -ts_verbose`.

## The Package Model

A directory is a package when its `tsconfig.json` lists a first-party file: a
path inside the repository and not under `node_modules`. `include`, `files` and
`exclude` are the program's, so they decide what the package compiles. A
directory without a local `tsconfig.json` can instead list existing manifest
entry points through the compiler. This includes separate declaration and
JavaScript exports and their imports; it does not synthesize a tsconfig.
Wildcard or nonlocal code entry points need an explicit tsconfig and produce
a diagnostic. Missing emitted entry points do not become source inputs. A
listing with no first-party file gets no target.

A `package.json` the lockfile has no importer for marks a foreign project:
pnpm installs nothing for it, so a bare import under it resolves through
whatever an importer above hoisted, or not at all. That directory and every
directory below it, down to the next `package.json` that is an importer, is
outside the package model: a `tsconfig.json` there is not listed and is no
package, and the run names the manifest once. A project meant to build on its
own is listed in `pnpm-workspace.yaml`.

A file belongs to the nearest package at or above its directory when that
package's program lists it. A file that package does not list belongs to no
package: every run from the repository root names each such file with the
programs that reached it, and so names a listed file under a directory the walk
did not enter (`# gazelle:exclude`, `.bazelignore`). A file
two programs list, a parent's and a nested package's, is the nested package's
alone, and the parent's edge to it is a dep.

An import into a source file with no indexed TypeScript target owner becomes
a direct `srcs` label in the consumer. This includes declarations-only
packages that generate no compile target. Gazelle follows that file's
compiler-observed imports, including transitive source and JSON inputs,
declarations, and npm type references. The compile rule enforces its supported
source formats and requires each direct npm dependency's store File to match a
nearest link on the consumer's chain or a retained source's importer chain.
Retained inputs whose observed npm lookups use different supplying importers than the consumer retain the source lookup context in `source_node_modules`. An empty intermediate importer adds no requirement; an ancestor importer can supply a declaration companion shadowed on the consumer's chain. Each context keeps its nearest package links and their declaration companions at their original importer paths. Different importer locations may use different versions of the same package; different store Files at the same actual link path are an error. A partial update requires the needed importer target to exist; otherwise, include its package in the Gazelle update. A kept `source_node_modules` must supply the required importer scopes.
Source-mode compiler owners, including explicit resolve owners, remain
dependencies while Gazelle follows their compiler-observed implementation imports
under the consuming target's configuration. Ordinary import traversal stops at
emitted compiler owners and generated outputs. A reached source kept
in this compilation's `srcs` still contributes its import closure. Labels use
the nearest existing Bazel package. Gazelle does not create exports, widen
visibility or include unrelated neighboring sources. A borrowed authored
TypeScript, JavaScript or declaration file also retains its nearest
`package.json` unless a compiler dependency supplies that original scope,
preserving package-private `imports` and module format. The manifest needs the
same eligibility and Bazel visibility as the file. Other runtime assets still
need an existing data owner.

Kept roots contribute the same compiler-observed closure whether discovery starts from a tsconfig or package exports. Gazelle expands direct source labels, literal native `filegroup.srcs`, literal `alias.actual` chains and declared output names from BUILD facts, normalizing each nested label in its own package while retaining the written source label. Generated outputs keep their existing producer boundary; Gazelle does not read stale generated contents to discover their imports.

A literal empty filegroup is known to contain no files. A `glob`, `select`, custom provider, non-default output group or unobserved external membership is unknown. If automatic closure requires it, Gazelle stops before writing BUILD files and names the compiler, forwarding label and unsupported attribute. Use explicit source-file labels, or keep the whole compiler rule (or ignore its package) and maintain its sources and deps manually. A kept attribute alone does not make unknown membership discoverable.

Automatic source and dependency updates also require an observed program under the selected compiler configuration. An external, generated, opaque or unavailable `tsconfig` does not establish an empty closure. Gazelle refuses the update before writing BUILD files, including for generated proto wrappers. Select an authored, readable configuration, or keep the whole rule (or ignore its package) and maintain its complete inputs and dependencies. Keeping every closure attribute also leaves those fields manual; keeping only `tsconfig` leaves them automatically managed.

An implementation import from a compiler owner requires a known `emit` mode to choose between that owner’s source closure and declarations. Gazelle reads absent or literal `emit` values; it does not evaluate expressions such as `select`. An unknown mode stops automatic closure before BUILD publication. Use a literal value, or keep the whole consuming rule and maintain its complete inputs and dependencies. Authored declaration boundaries use their observed closure in either mode; they do not require this mode decision.

Gazelle checks the declared compiler-owner graph for an updated binary or Node test. Each reached compiler owner must establish `emit = True` or expose known sources that need no TypeScript transformation. This applies inside the update too when a keep marker prevents promotion. Dependencies and forwarding labels must be literal BUILD facts; a `select`, variable or other expression cannot establish the closure even for an emitted owner. Gazelle refuses before writing any BUILD file. Use literal labels and emission settings, or keep the whole consuming rule and maintain its runtime closure manually. A borrowed declaration supplies typing; its runtime implementation must be explicitly included or supplied by an owner. An observed consumer outside the update adds no demand until it reaches a regenerated compiler owner.

A borrowed authored module cannot use a generated `package.json` as its
package scope: the checker reads the module at its source path, while the
manifest exists at its output path. Gazelle reports this conflict whether or
not a stale manifest exists in the checkout. Keep the manifest authored, or
generate the module and manifest together in the same output layout. Explicit
imports of generated JSON still use the declared output file.

BUILD output declarations are recorded before metadata drives package or emission
discovery. Generated manifests supply neither authored emission roots nor foreign
project markers, npm self-reference names or manifest dependency unions. Authored
programs under such a scope report the same layout conflict with or without a
checkout copy. Automatic membership also cannot be discovered through a generated
`tsconfig.json` or generated relative `extends` file: generation reports a conflict
before writing BUILD files. Use authored discovery metadata, or select a generated
config under another filename on an explicitly kept compiler rule. The latter
remains a declared File input; Gazelle does not inspect its checkout copy.

Compiler wildcard discovery excludes generated outputs before choosing between same-stem extensions. Native protobuf outputs participate when the provider index completes, so a checkout copy of `value_pb.ts` cannot hide an authored `value_pb.js` root or its dependencies. Gazelle then reapplies kept roots and refreshes membership from the new compiler observation. The relisted closure still uses Gazelle's native directory and exclusion facts: an excluded or unavailable required input reports a conflict before BUILD files are written.

Discovery understands literal `out`/`outs` names and literal `ts_codegen.out_dir`
values; implicit outputs hidden inside a macro are outside this discovery contract.
A computed explicit declaration leaves other paths in its Bazel package unknown;
Gazelle does not evaluate Starlark. Unknown metadata cannot become authored merely
because a checkout file appears. Optional discovery is omitted with a diagnostic,
without creating or withdrawing TypeScript rules. A required config, relative
`extends`, inherited Vitest config selection, package scope or file input reports a
conflict, as does discovery that
would withdraw an existing unkept compiler owner. Explicit producer labels and
kept or independently named targets retain their existing Bazel contracts.
A known literal producer remains usable beside an unknown declaration, and a
separate BUILD package has its own provenance boundary.

Optional application discovery also ignores generated `index.html` checkout
copies; authored HTML and the existing application entry names still select a
development server.

When a test's closure adds any label to `srcs`, Gazelle also writes `test_srcs`
from its original test roots. Only those roots become runner entries
and shard inputs; the complete helper closure remains available for compilation
and runtime imports. Removing the extra labels removes the
redundant `test_srcs` selection.

Direct TypeScript, JavaScript and imported JSON outside the consumer's directory
tree retain their paths in an all-source closure (`emit = False`) within the workspace. A source-mode consumer of a relocated dependency uses the shared runtime layout while preserving its TypeScript bytes and original compiler source identities.
A workspace member's npm store rejects published Files outside the member directory,
where it cannot preserve relative imports. Move those files inside the member
or import them through a separately published package; adding a compiler owner
alone does not repair the npm layout. This restriction includes foreign
passthrough declarations left at their original paths, even when a type import can be
erased from an implementation. `srcs` has no checker-only declaration mode:
its declarations are published interface inputs, including possible ambient
effects and references from the emitted interface. Gazelle retaining such an
input does not establish that the member can be staged in the npm store.
Emitted `ts_compile` and `ts_test` programs preserve relative module paths under a common logical source root in the compiler's output namespace. The producer records exact source/runtime and source/declaration File pairs, including generated inputs. Consumers of relocated dependencies place links to their canonical outputs; unbundled binaries execute those published Files. Checked tsgo declarations support the same mixed-source layout. Source-mode consumers follow relocated dependencies without emission; all-source closures retain original paths. See [Shared Source Layout](../rules/ts-compile.md#shared-source-layout).

A workspace member can publish self-contained emitted outputs from its own borrowed sources. Its npm store rejects canonical module and declaration links selected for copying because the copy would lose the dependency's package scope and npm importer. Publish the dependency separately through npm or include its sources in the member.

Only runtime roles constrain runtime placement: a declaration's extra `package.json` in `type_inputs` remains compiler-only. Generated JavaScript, JSON and declarations retain their canonical producer Files when dependency links are placed.

A compiler-observed import of a file excluded by Gazelle stops generation
unless a declared generator owns it. `# gazelle:ignore` leaves a BUILD file
untouched: eligible imports from that directory still resolve, but its unimported data files do
not become inputs of an ancestor TypeScript target.

Within a package, a `*.test.*` or `*.spec.*` file is a test file, a `.d.ts`,
`.d.mts` or `.d.cts` a declaration, and every other listed file a library
file. A file tsgo could have listed -- `.ts`, `.tsx`, `.mts`, `.cts`, `.js`,
`.jsx`, `.mjs`, `.cjs` -- is a src only when the program lists it: one the
tsconfig's `exclude` leaves out is neither a src nor data. Every other regular
file under the package's tree -- not a `BUILD.bazel`, `BUILD` or `package.json`,
not the package's own `tsconfig.json`, not under a deeper package -- is a data
src of the package: a `.json`, a `.css`, an image or a fixture. Metadata-only
package manifests use `package_scopes` for runtime modules or `type_inputs` for
declarations; an explicit JSON import keeps the manifest in `srcs`.

A declared scalar output remains generated even when a local run leaves a copy
on disk. Gazelle leaves outputs declared by `out` or `outs`, including a
`genrule`'s, out of ordinary compiler membership. An imported output retains its
existing compiler owner, found through `srcs` or an explicit `gazelle:resolve`.
Without one, supported
scalar files can become direct `srcs`; `ts_codegen` dependencies also supply
their runtime and declaration outputs. Gazelle does not traverse stale generated
files' imports. Scalar output selection requires the compiler's
[file probes](../rules/ts-codegen.md); a skipped missing output directory cannot
supply that identity. The same guide names the supported formats and declaration
emission constraints.

An eligible JavaScript twin beside an owned declaration is also a src of that
owner when they share the same stem (`x.mjs` beside `x.d.mts`): tsc drops it from the program and
resolves `./x.mjs` to the declaration, so the listing never names the module
itself. A retained foreign declaration keeps its twin, with that module's runtime package scope, only when the twin's `exports_files` makes it visible to the consumer. Otherwise Gazelle does not infer runtime imports from an import of a declaration
or discover independently authored JavaScript hidden behind it. Such runtime
inputs need compiler-observed membership or an existing declared owner that
supplies them. Missing implementations are rejected by validation or the runtime
consumer when required.

Two programs importing each other's files is a dependency cycle between two
Bazel targets, which Bazel rejects with the loop of labels when it loads them.
Directories inside one program are one target, so a mutual import between them
is nothing.

### Where a Program Is Not

A directory that is not a package gets `Empty` for every kind Gazelle writes --
`ts_compile`, `ts_test`, `ts_config`, `filegroup(vitest_config)` and
`filegroup(wrangler_config)` under the names it would use -- so a rule an
earlier run left there is withdrawn, and the run names the BUILD file to delete
when one of them was in it: Gazelle cannot delete the file, and an empty BUILD
file keeps the directory a Bazel package. Two things are written there all the
same: a `ts_config` over a `tsconfig.json` a program's `extends` chain names
(the root's shared base), and at the repository root the filegroups over a
config the tests below name and over the wrangler config it names. A directory
inside a `ts_codegen`'s `out_dir` is that target's output: nothing under it is
a source, and a BUILD file there is emptied and named the same way.

## What Gazelle Writes

| Rule | Name | Attributes Gazelle owns |
|------|------|-------------------------|
| `ts_compile` | the directory's basename, `root` at the repository root | `srcs`, `deps`, `tsconfig`, `visibility` |
| `ts_test` | `<basename>_test` | `srcs`, `deps`, `tsconfig`, `config`, `config_srcs`, `config_node_modules`, `workers_pool`, `wrangler_config`, `coverage_provider` |
| `ts_config` | `tsconfig` | `src`, `deps`, `visibility` |
| `ts_dev_server` | `dev` (`dev_server` when the compile target is `dev`) | `entry_point`, `plugin`, `node_modules`, `visibility` |
| `filegroup` | `vitest_config` | `srcs`, `visibility` |
| `filegroup` | `wrangler_config` | `srcs`, `visibility` |
| `node_modules` | `node_modules` | `deps`, `parent`, `hoist`, `visibility` |
| `node_modules_member` | `node_modules/<member name>` | `member`, `visibility` |
| `npm_virtual_store` | `node_modules/.pnpm` | |

Per package: a `ts_compile` when the program has a library file, holding the
library files, every owned declaration and the data files; a `ts_test` when it
has a test file, holding the test files and every owned declaration, and the
data files when no `ts_compile` is written; `tsconfig = ":tsconfig"` on both,
naming the `ts_config` over the package's own `tsconfig.json`. A hand-written
`ts_codegen` in the package's BUILD file is a dependency of every target Gazelle
writes there; Gazelle never writes one. A program listing only declaration files
writes neither target and says so under `-ts_verbose`. An application with non-test program
sources and a package index.html or listed main.ts[x]/app.ts[x] also gets a
default oj dev target. Gazelle updates its entry point, npm tree, plugin and
visibility, and removes a generated dev target when its application entry
disappears. It preserves `server`, `port`, `host`, `open` and `# keep` values
([Dev Server](../guides/dev-server.md)).
A src whose name Bazel cannot spell (a `:` in it) is dropped and named in the
log; a name that opens a label (`@`, `//`) is pinned to the package with a
leading `:`.

Per lockfile importer, package or not: a `node_modules` whose `deps` are the
importer's declared `dependencies`, `devDependencies` and
`optionalDependencies` as hub labels (`@npm//web:react`; `@npm//:react` for
the root's) and whose `parent` is the importer above's target, the
lockfile's root importer naming `hoist = ":node_modules/.pnpm/node_modules"`
instead; one
`node_modules_member` per `link:` entry, `node_modules/<member name>` over the
member's view; and, at the repository root, the lockfile's package,
`npm_virtual_store(name = "node_modules/.pnpm")` from `@npm//:defs.bzl`
([node_modules](../rules/node-modules.md)). A directory that is no importer
withdraws its `node_modules` and every `node_modules_member`; a hand-written
one there is kept under `# keep`.

```python
# packages/core/BUILD.bazel -- tsconfig.json here, the sources under src/
load("@rules_typescript//ts:defs.bzl", "ts_compile", "ts_config", "ts_test")

ts_compile(
    name = "core",
    srcs = [
        "src/index.ts",
        "src/parse.ts",
    ],
    package_scopes = ["package.json"],
    tsconfig = ":tsconfig",
    visibility = ["//visibility:public"],
    deps = ["@npm//packages/core:zod"],
)

ts_test(
    name = "core_test",
    srcs = ["src/parse.test.ts"],
    tsconfig = ":tsconfig",
    deps = [
        ":core",
        "@npm//packages/core:vitest",
        "@npm//packages/core:zod",
    ],
)

ts_config(
    name = "tsconfig",
    src = "tsconfig.json",
    visibility = ["//visibility:public"],
    deps = ["//:tsconfig"],
)
```

A `ts_test` runs under the vitest config plain `vitest` would read for its
files: a `vitest.config.*`, else a `vite.config.*`, in vitest's own order of
extensions (`.ts`, `.mts`, `.cts`, `.js`, `.mjs`, `.cjs`), so a package that
configures vitest through vite -- a `define`, a plugin, a `resolve.alias` --
runs as `pnpm vitest` runs it. One beside the tests goes into `config` by
name. With none there, Gazelle walks up to the directory plain `vitest` runs
from, the nearest one holding a `package.json` or the repository root, and
takes the config it holds: that directory gets a public `filegroup` named
`vitest_config` over the file, and the test names it by label. A directory
between the two with no `package.json` is passed over, as `vitest` run from the
package root passes it over. The directory holding the config has to be a
package itself, or the root, for the label to reach it; otherwise the run says
so and the test gets no `config`. Without the config the tests run in plain
Node, so a worker's `defineWorkersConfig` pool becomes no pool and a dependency
that only resolves through Vite (`test.server.deps.inline`) fails at import
time. The config is listed by tsgo from its own directory, as vitest loads it,
and the listing is followed through first-party source modules, stopping at
declared generated-output owners and workspace-member packages. The declaring importers or member links of its npm packages go in
`config_node_modules`, whose links and store closures use the same runfiles
staging as `data`. Gazelle leaves authored `data` unchanged, including importer
targets, and recomputes `config_node_modules` when imports or config selection
change. Test-program npm deps and the test's importer chain stay their own;
first-party runtime owners remain in `deps`. A stale generated file's imports
contribute no config inputs or dependencies. Source modules remain the test's
`config_srcs`, spelled from the test's package -- a path under it, or
`//<package>:<file>` for a config
an ancestor package exports -- and the
rule writes each at its own path in the runfiles, where the config's relative
imports resolve ([A config file](../rules/ts-test.md#a-config-file)). A module
outside the config's package is said and gets no entry: a config's modules are
its package's files.

### A Workers-Pool Config

A config that imports `@cloudflare/vitest-pool-workers` runs its tests inside
workerd, and the pool reads the wrangler config `wrangler.configPath` names --
that key alone; a `wrangler.jsonc` beside a config naming none is nothing to
it. Gazelle reads the one string literal in the config matching
`wrangler[\w.-]*\.(jsonc|json|toml)` and makes the file a label in the
config's package: `filegroup(name = "wrangler_config")` beside `vitest_config`,
withdrawn when the literal or the file goes. A config naming two files, or a
file outside its package, is said and gets none. The `ts_test` whose config's
edges name the pool gets `wrangler_config` -- the filegroup's label from a
package below, the file's name from the config's own -- and, when the lockfile
declares `@vitest/coverage-istanbul`, `coverage_provider = "istanbul"` with
that package in `deps` in the importer's spelling; the pool refuses v8
coverage, and without the package the run says so and writes neither. The
filegroup follows the literal and is written when the package generates; the
attributes follow the pool's edge and are written with `deps`, from the
config's listing. A config that names a wrangler config and installs no pool
gets the filegroup and no test names it.

`workers_pool` is the `node_modules` importer or `node_modules_member` link from the resolved pool import,
including an import in a relative helper below the entry config. It selects
the pool whose Wrangler prepares `wrangler_config` and stages that pool for
the runtime config. The importers in `config_node_modules` remain the config's
whole npm closure, including workspace-member links. Importers of the same full
pool resolution share preparation: all runtime links remain staged, and Gazelle
selects a stable owner. Different versions or peer resolutions require separate
test targets because one Wrangler action uses one parser. Without Wrangler preparation no selection is emitted. Removing
the pool import withdraws `workers_pool` on the next run.
Existing hand-written tests that omit `workers_pool` keep their declared test
chain's pool selection; the generated explicit owner takes precedence.

## The tsconfig and Its ts_config

Every target compiles under the package's own `tsconfig.json`: its `lib`,
`types`, `paths`, `jsx` and strictness. The rule reads every compiler option
from the tsconfig ([where compiler options come
from](../rules/ts-compile.md#where-compiler-options-come-from)), so no
attribute restates one to the compiler. The `ts_config` beside the file is what
makes it a label, and it declares what the rule needs from the file before any
action reads it: `deps`, the `extends` chain, and the two values that name an
output -- `jsx = "preserve"` when that is the chain's effective `jsx`
([`jsx: preserve`](../rules/ts-compile.md#a-tsx-under-jsx-preserve)) and
`module` when the chain's is one tsgo emits
([The Module Format](../rules/ts-compile.md#the-module-format)). All three
are Gazelle's to recompute on every run.

`ts_config.jsx` is written as `"preserve"` when the chain's effective `jsx` is
`preserve`, read leaf-wins as tsc reads it and inherited through `extends`, and
removed otherwise; no other value is written, since no other value names an
output. `ts_config.module` is written the same way when the chain's effective
`module` is one tsgo emits -- `commonjs`, `node16`, `node18`, `nodenext`,
lowercased as tsgo prints it -- and removed for an ES kind or `preserve`,
which oxc emits with no twin to name.

`ts_config.deps` is the `extends` chain. Gazelle reads the package's
`tsconfig.json` and writes a dep on the `ts_config` of every `tsconfig.json`
its `extends` names, one specifier or an array, each a relative path resolved
from the file. A `tsconfig.json` that is no program of its own gets a
`ts_config` in its directory because a chain names it, which is how the root's
shared base becomes `//:tsconfig`. A base of another name
(`tsconfig.base.json`) has no `ts_config` to name, and the run says so: declare
that dep by hand under `# keep`. A specifier that resolves through
`node_modules` (`"@tsconfig/node20/tsconfig.json"`) is skipped with a warning,
since only configs on disk are read. Without the dep the base is no input to
the action, and tsgo reports `TS5083: Cannot read file` before it reaches the
sources.

```python
# workers/proxy/test/BUILD.bazel; its tsconfig.json extends ../tsconfig.json
ts_config(
    name = "tsconfig",
    src = "tsconfig.json",
    visibility = ["//visibility:public"],
    deps = ["//workers/proxy:tsconfig"],
)
```

`deps` is Gazelle's, recomputed on every run, so a run corrects the label when
the base moves or goes away. A hand-written value needs a `# keep` on its line
to survive the next run ([attributes Gazelle
owns](directives.md#attributes-gazelle-owns)).

### A Declaration the tsconfig Names

A path-shaped `compilerOptions.types` entry, `"./worker-configuration.d.ts"` in
the form wrangler writes, names a file the program stages, and the rule reads
the entry from the tsconfig itself. The listing carries it as an edge from the
tsconfig to the file, and it resolves as any edge does: to the rule whose
`srcs` hold the file, or to the `ts_codegen` whose `outs` declare it. An out
that is not in the checkout is listed by no program, so Gazelle reads the entry
from the chain itself, resolved against the package's directory as tsc
resolves it, and writes the codegen into `deps`. Nothing else is written: no
`types`, no filegroup. A copy of a declared out left on disk by a local
`wrangler types` is no rule's src: the out is the codegen's, and the program
that lists it depends on the codegen. The codegen is named for what it writes,
never for its directory: the directory's name is the package's `ts_compile`,
and a hand-written rule of another kind holding that name keeps the merger
from writing the compile. The run names such a rule; rename it.

```python
# workers/proxy/BUILD.bazel -- the ts_codegen is hand-written; Gazelle leaves it
ts_codegen(
    name = "worker_types",
    srcs = ["wrangler.jsonc"],
    outs = ["worker-configuration.d.ts"],
    args = ["--config", "wrangler.jsonc", "--out", "{out}", "--srcs", "{srcs}"],
    generator = "@rules_typescript//tools/codegen:wrangler_types",
    node_modules = ":node_modules",
    visibility = ["//workers/proxy:__subpackages__"],
)

ts_compile(
    name = "proxy",
    srcs = [
        "src/handler.ts",
        "wrangler.jsonc",
    ],
    tsconfig = ":tsconfig",
    visibility = ["//visibility:public"],
    deps = [":worker_types"],
)
```

## Import Resolution

`deps` is written from the listing, one label per edge target. An edge is one
file reaching another: an import as written, a `declare module "<name>"`
augmentation of a module the program holds, a `/// <reference path>` or
`/// <reference types>` directive, a `compilerOptions.types` entry, the JSX
runtime a `.tsx` file imports under `react-jsx` without writing the import, the
`importHelpers` module. An augmentation adds no file -- its module is in the
program by an import elsewhere, or the checker reports `TS2664` -- and tsgo
lists it (`Augmented via`) under [the source-built
compiler](../rules/providers.md#tsgo-from-source), not the lockfile's binary.
tsgo resolved each under the program's own options --
its `paths`, its `moduleResolution`, the `imports` map of its `package.json`,
the `exports` maps under `node_modules` -- so Gazelle reads no specifier and
applies no resolution rule of its own. It maps the file tsgo landed on to a
label:

- **A file under `node_modules`** is an npm package: the bare specifier's
  package when the edge is an import and the lockfile mentions that name, else
  the package the path names by the segments after its last `node_modules/`
  (two when the first is a scope). The label is spelled through
  [the lockfile gate](#the-lockfile-gate). The `@types/*` twin of a package
  the lockfile mentions is the importer's to declare beside it and gets no
  label of its own; a `@types/<name>` package installed with no `<name>`
  beside it is the label, `@npm//web:types_mdast`.
- **A `/// <reference types>` directive in a file under `node_modules`** is the
  target's edge when the file it landed on is the `@types/<name>` package an
  importer at or above the target's package declares, spelled as that
  importer's, `@npm//:types_node`: TypeScript's primary lookup for the directive
  walks `node_modules/@types` up from the tsconfig's directory -- the chain --
  before the referencing file's own directory, so that copy is the one the
  program loads, and the link has to be staged for the build's program to load
  it too. A directive the chain does not answer resolved beside the referencing
  package's own tree and is nothing. The directive is the edge of the rule
  whose files reach the referencing file, the `ts_compile`'s or the `ts_test`'s.
- **A workspace member imported by its name** -- a bare specifier whose
  package is a `link:` name in the lockfile, or the manifest name of an
  importer -- is the name as the nearest importer at or above the package
  that has it resolves it: the importer's link target where it links the
  member, `//web:node_modules/@acme/ui`; the importer-scoped label where it
  declares the name with a version, `@npm//packages/bundler-plugin:example-transform`
  for the bundler plugin's `"example-transform": "1.1.13"` beside the member
  `packages/transform`, since pnpm installs the published package there;
  where no importer above links or declares it there is no label and one
  line names the member, since a target resolves through its importers
  alone. The name of the nearest
  `package.json` above the importing file, a subpath included, is a
  self-reference, which tsc resolves through that manifest's `exports` to a
  file of the member: a first-party file, resolved as any owned file is -- the
  member's `ts_compile` from a `ts_test` or a package below it, nothing from
  the member's own `ts_compile` -- and no line.
- **An owned file**, reached by a relative path or a `paths` alias, adds no
  dependency when the importing rule itself owns it. Another `ts_compile`
  owner exports `TsInfo` and remains a dependency, including within the same
  package; a hand-written owner under `# keep` answers as a generated one does.
  Membership in a `ts_test` does not export a library.
- **A file under a `ts_codegen`'s `out_dir`** is the codegen, matched by the
  root the path sits under, deepest root first across indexed providers and
  observed BUILD declarations. Native providers take precedence at the same root;
  declared roots remain available with partial indexing or `-index=false`.
  Ordinary imports and config imports retain that generated
  identity, so checkout copies and their stale imports never become borrowed
  source inputs.
- **A scalar generated output** keeps its declared compiler owner before and
  after generation, even when excluded. Without one, supported files enter
  `srcs` directly; declarations and JSON from `ts_codegen` instead
  reach consumers through its provider. Direct TypeScript and JavaScript inputs
  from `ts_codegen` retain the producer dependency for its runtime and
  declaration outputs.
  Scalar lookup follows compiler extension precedence; see
  [generated-output resolution](../rules/ts-codegen.md).
- **An eligible source with no indexed target owner** becomes a direct `srcs`
  label and contributes its compiler-observed import closure, under the
  source-mode restriction above. Declaration-only packages can supply these inputs too.
  Gazelle rejects excluded imports unless a declared generated-output owner
  supplies them; unsupported or unlabelable files receive a diagnostic.
- **The compiler's own libs** (`lib.dom.d.ts` and its kin: under `../` from
  the lockfile's binary, which reads them from beside itself, under
  `bundled:///libs/` from the source build, which embeds them) are nothing.

The tsgo action checks every edge against `deps` from the same listing
([Deps Have to Be Direct](../rules/ts-compile.md#deps-have-to-be-direct)), so
a build over what Gazelle wrote has no undeclared import to report;
`//tests/integration:gazelle_roundtrip_test` builds Gazelle's output and pins
it.

Core Gazelle's `# gazelle:resolve typescript <repository path> <label>` names
the label for a first-party file and wins over every first-party case above; a
file under `node_modules` is the gate's alone
([`# gazelle:resolve`](directives.md#gazelleresolve)).

A `ts_test`'s `deps` is the union of `:<basename>`, the package's `ts_compile`
when there is one; the edges of every file the package owns -- the test files,
the library files and the declarations, since the test runs the package's code
and needs its npm closure; the edges of its vitest config and of the modules
the config reaches; and the nearest
`package.json`'s `dependencies` and `devDependencies`, each spelled as an edge
would be, a member's name as its link target and every other name through the
gate.
So a test carries the packages the config and the manifest name and no source
imports (`vitest`, a pool package, `jsdom`), and none of them needs a `# keep`.

`runner` and `data` are the owner's attributes: Gazelle writes neither, and a
hand-written value survives every run without `# keep`
([Runners](../rules/ts-test.md#runners)).

Gazelle recognizes the built-in Node runner through apparent and canonical
labels, including aliases. Canonical recognition uses the repository containing
the extension: build it from your `rules_typescript` module dependency. A prebuilt
extension from an arbitrary repository does not establish canonical runner
ownership in another workspace.

### The Lockfile Gate

Every npm label passes through the root `pnpm-lock.yaml`. An edge the lockfile
never mentions, under the specifier's package or the listed file's -- one a
nested `package-lock.json` installed, one a stale store supplied -- gets no
label and one line naming the importer, the specifier and the name. The hub is
built from that lockfile, so `@npm//:<name>` for such a package is a target
that cannot exist, and `no such target` fails analysis for every target in the
build where a missing dep fails one import with `TS2307`.
With no root lockfile at all, npm imports get no dep, said once per run.

The spelling follows the importer. The file tsgo listed carries the exact
version the importing file's own `package.json` resolved, and the hub declares
each importer's resolutions under the importer's directory beside the root's:
`@npm//web:marked` beside `@npm//:marked`. A name is spelled under the nearest
importer on the chain above the importing file that declares it, the root
last. A flat label fails analysis when no declared importer context selects
its store File as the nearest binding. Every `ts_compile` and `ts_test` gets
`node_modules`, the nearest lockfile importer's target at or above the package -- the root's for a
package under no importer -- which is that chain.

## Verifying a Run

`//tests/integration:gazelle_roundtrip_test` pins four properties against a
nested workspace: the output builds, generating it twice from scratch produces
byte-identical BUILD files, `bazel test //...` passes on that output, and the
set of test targets is unchanged across a delete-and-regenerate. A run that
deletes a test still builds and is still idempotent, so check the test-target
set on your own repository:

```bash
bazel query 'tests(//...)' | sort > before
bazel run //:gazelle
bazel query 'tests(//...)' | sort | diff before -
```

Gazelle's Go language, in `gazelle_ts`, turns `# gazelle:exclude *_test.go`
into a deletion stub named `<dirbase>_test`, and a hand-written `go_test` of
that name goes with it.

### Getting the Clean-Tree Diff to Empty

Once a repository has settled, a Gazelle run on an unmodified checkout should
change nothing. Check without writing anything:

```bash
bazel run //:gazelle -- -mode=diff
```

Three things commonly keep that diff non-empty on a hand-written BUILD file,
and none is drift:

- **Gazelle's own rendering.** It writes a one-element list inline
  (`deps = ["//pkg"]`) and names a generated file by its producing label where
  you wrote the filename. Reformat the file to match.
- **A hand-written rule under a name Gazelle would use.** In a directory that
  is no package, every rule Gazelle would write is withdrawn under the names it
  would use -- `<dirbase>`, `<dirbase>_test`, `tsconfig`, `vitest_config`,
  `wrangler_config` -- so a rule of yours under one of them is proposed for
  deletion on every run. `# keep` above the rule holds it.
- **A hand-narrowed attribute it merges.** `visibility` is a merged attribute
  and generated rules carry `//visibility:public`, so a target restricted to
  `["//myapp:__subpackages__"]` comes back public on every run. Pin it with
  `# keep`:

  ```python
  ts_compile(
      name = "internal",
      srcs = ["index.ts"],
      # keep
      visibility = ["//myapp:__subpackages__"],
  )
  ```

  `# keep` is Gazelle's own directive. Above an attribute it means "never
  touch this value"; above a whole rule, "never touch this rule". See the
  [Directives Reference](directives.md).

## Exported Files at New Package Boundaries

When ordinary generation creates a child package, existing literal
`exports_files` entries beneath that boundary move to the canonical child
owner. Visibility and licenses remain unchanged. Consumers must use the new
child label; Gazelle does not add compatibility aliases. If required exports
are kept or have computed visibility or licenses, Gazelle refuses the update
before writing any BUILD files. Remove the keep marker or use literal
attributes before creating the child package.

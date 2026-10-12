# Troubleshooting

## No repository visible as '@rules_rust'

```
ERROR: @rules_rust//rust/toolchain/channel :: Error loading option
@rules_rust//rust/toolchain/channel: No repository visible as '@rules_rust'
from main repository
```

Remove old `--@rules_rust//...` flags from the consumer's `.bazelrc`. rules_typescript uses rules_rs and hermetic LLVM; it does not expose a rules_rust compatibility repository. For consumer Rust code, declare rules_rs and configure its toolchain extension directly. Cargo hubs remain independently named, so the consumer can use `crates` alongside the ruleset's hubs.

## No repository visible as '@pnpm'

```
ERROR: no such package '@@[unknown repo 'pnpm' requested from @@ (did you mean
'npm'?)]//': The repository '@@[unknown repo 'pnpm' requested from @@ (did you
mean 'npm'?)]' could not be resolved: No repository visible as '@pnpm' from
main repository and referenced by '//:pnpm'
```

The root `BUILD.bazel` holds a `ts_pnpm(name = "pnpm")` or a
`ts_add_package`, and both name `@pnpm`. Take the repo:

```python
npm = use_extension("@rules_typescript//npm:extensions.bzl", "npm")
npm.translate_lock(pnpm_lock = "//:pnpm-lock.yaml")
use_repo(npm, "npm", "pnpm")
```

`npm.pnpm(version = …)` is optional; a default version is used. See
[npm Dependencies § Setup](npm.md#setup).

## No repository visible as '@npm'

```
ERROR: no such package '@@[unknown repo 'npm' requested from @@]//': The
repository '@@[unknown repo 'npm' requested from @@]' could not be resolved:
No repository visible as '@npm' from main repository and referenced by
'//src/lib:lib_test'
```

Gazelle resolved a bare import to an `@npm//:…` label and `use_repo` does not
take `"npm"`. One npm dependency is enough to need the extension: the three
lines above. A label naming a second hub (`@npm_tools//:…`) means that hub is
missing from `use_repo`; see [more than one hub](npm.md#more-than-one-hub).

## No test targets were found, yet testing was requested

```
INFO: Found 2 targets and 0 test targets...
ERROR: No test targets were found, yet testing was requested
```

`bazel test //...` exits 4 when the pattern matches no test target, and a project
with no `*.test.ts` / `*.spec.ts` yet has none. Gazelle generates a `ts_test` from
the first such file it sees; vitest comes from your lockfile.
[The quickstart's step 8](../getting-started/quickstart.md#path-a-new-project)
walks the first-test path.

## No tsgo or Tools Toolchain / Declarations Are Missing

Toolchain registration is the consumer's job. Your `MODULE.bazel` needs:

```python
register_toolchains("@rules_typescript//ts/toolchain:all")
```

Without it nothing resolves the tools toolchain, so every `ts_compile` fails
toolchain resolution naming `//ts/toolchain:tools_toolchain_type`, and nothing
resolves the tsgo toolchain, so under the default `--//ts:declarations=tsgo`
there is no `.d.ts` and no type-checking. The Go tools are built from source with rules_go. The helpers run on the execution platform; the launcher runs on the target platform. A tools release is needed only when the root module explicitly selects [prebuilt tools](../RELEASE_PROCESS.md#tools).

To choose the compiler, point the `ts` extension at the lockfile that pins
`typescript` (7 or later):

```python
ts = use_extension("@rules_typescript//ts:extensions.bzl", "ts")
ts.tsgo(pnpm_lock = "//:pnpm-lock.yaml")
```

`ts.tsgo(version = "7.0.2")` downloads a named release unverified instead. Both
are in [Version Pinning](../getting-started/quickstart.md#version-pinning).

Windows is not supported, so no tsgo, tools or launcher toolchain resolves
there: a `ts_test`, `ts_binary`, `ts_dev_server` or `npm_bin` for a Windows
target platform fails analysis naming the platform. See
[COMPATIBILITY.md](https://github.com/mikn/rules_typescript/blob/main/COMPATIBILITY.md#windows).

## npm: pnpm-lock.yaml declares patchedDependencies with no patch file

Your lockfile patches a package and the extension was not given the patch. Pass
it as a label:

```python
npm.translate_lock(
    pnpm_lock = "//:pnpm-lock.yaml",
    patches = ["//patches:@acme__diffs@1.3.1.patch"],
)
```

A patch file that no `patchedDependencies` entry claims fails too, and means the
lockfile is stale or the file is misnamed. Names follow `pnpm patch-commit`:
`<name with / replaced by __>@<version>.patch`. See
[npm Dependencies](npm.md#patched-dependencies).

## npm: patch labels that resolve to no readable file

```
npm: patch labels passed to npm.translate_lock(patches = [...]) that resolve to
no readable file:
  @@//patches:@acme__diffs@1.3.1.patch
```

The label is wrong, the file is missing, or the Bazel package holding it failed
to load. A patch filename starting with `@` causes the last: `glob()` prefixes
`:` onto the result and `exports_files` rejects it as a target name, failing the
whole package and every patch in it. List those files literally:

```python
exports_files(["@acme__diffs@1.3.1.patch", "nanoid@3.3.11.patch"])
```

## npm: patch files whose sha256 disagrees with the lockfile

```
npm: patch files whose sha256 disagrees with the one pnpm-lock.yaml records in
patchedDependencies:
  @@//patches:@acme__diffs@1.3.1.patch
    lockfile: 384aa81a…
    file:     19bbd346…
```

pnpm writes that digest when it writes the patch, so a disagreement means the
patch file changed without `pnpm install` being re-run. Re-run it (`pnpm install
--lockfile-only`) so the lockfile records what the file now is, or restore the
file. Bazel does not apply a patch pnpm never saw.

## node_modules: linked twice

```
node_modules: @@//src/app:node_modules: 'minimatch' linked twice, to
@@+npm+npm__minimatch__10_2_4//:pkg and @@+npm+npm__minimatch__9_0_9//:pkg:
one link per name
```

`deps` names two resolutions of one name, and `node_modules/<name>` is one
link. Depend on the one the importer declares; a dependent that resolved the
other reaches it beside its own store tree. Two peer resolutions of one version
are two keys and fail the same way. See
[node_modules](../rules/node-modules.md#one-link-per-name).

## imports files no direct dep provides

```
ERROR: .../src/app/BUILD.bazel:3:11: TsgoCheck //src/app:app failed: (Exit 1)
tsaction: //src/app:app imports files no direct dep provides:
  src/app/main.ts imports "zod"
    resolved to node_modules/zod/index.d.ts
    add @npm//:zod to deps
```

The import resolves today only because it reaches this target through another
dep's own deps, and stops resolving the moment that dep drops it. Add the label
the message names, or run Gazelle:

```bash
bazel run //:gazelle
```

Gazelle writes `deps` from tsgo's own listing of the program, the listing the
check reads, so a Gazelle run that leaves a reported failure unfixed is a bug.
Outside the check: an edge from a dep's own file, which is that dep's to
declare; a tsconfig `types` entry; and an import nothing in the closure
provides, which TypeScript reports as `TS2307`.

## Option 'baseUrl' has been removed

```
pkg/tsconfig.json(3,5): error TS5102: Option 'baseUrl' has been removed.
Please remove it from your configuration.
```

`baseUrl` is gone in tsgo, and the generated config cannot take it back out of
your file: the diagnostic fires on the key being present anywhere in the
`extends` chain, so re-stating it above yours only moves the error. Delete it
from the `tsconfig.json` the target names. `paths` here is Bazel's, rewritten
per configuration, so nothing in this ruleset resolves against a `baseUrl`.

## Option 'moduleResolution' must be set to 'NodeNext'

```
pkg/tsconfig.json(2,3): error TS5109: Option 'moduleResolution' must be set to
'NodeNext' (or left unspecified) when option 'module' is set to 'NodeNext'.
```

Two files each supply one half of a coupled pair. The ruleset's baseline states
no `moduleResolution`, so a `module` of yours never has a stray baseline
resolver under it. Your own `extends` chain still can: a `module` in one file
and a `moduleResolution` in another. Put both halves in one file, or drop the
`moduleResolution` and let tsgo derive it.

The mirror image, `TS5110`, is a `moduleResolution` of `Node16`/`NodeNext` with
no `module` beside it.

## Import Not Resolving in tsgo

tsgo resolves with `moduleResolution: "Bundler"` (what tsgo derives from every
`module` but `Node16`/`NodeNext`) through the importer chain `node_modules`
names. A bare import that resolves nowhere, with no strict-deps failure and
only `TS2307`, means no dep provides it. Add the package, which the importer's
`package.json` declares -- a name no importer on the chain declares, or one
declared at another version, fails analysis naming the manifest or the label
to write:

```python
ts_compile(
    name = "app",
    srcs = ["app.ts"],
    deps = ["@npm//:zod"],
)
```

## Cannot find type definition file for 'vite/client'

```
app.ts(1,23): error TS2688: Cannot find type definition file for 'vite/client'.
```

From a `/// <reference types="vite/client" />`, the line Vite's own project
template puts at the top of `src/vite-env.d.ts`. The directive resolves through
TypeScript's type-reference resolver, which walks `node_modules/@types` and the
`node_modules` above the file; under Bazel that walk is the importer chain, so
the package has to be a dep the importer declares:

```python
ts_compile(
    name = "app",
    srcs = ["src/main.ts", "src/vite-env.d.ts"],
    deps = ["@npm//:vite"],
)
```

Gazelle writes that dep from the listing: the directive is an edge from
`vite-env.d.ts` to the package's file, resolved by tsgo, and the package it
landed in is the label
([Import Resolution](../gazelle/overview.md#import-resolution)). A whole tree
that needs a package names it in the `types` of the tsconfig its packages
extend, which is an edge of every program in the tree.

That is the directive in a file of your own: Gazelle writes the dep it names,
and the tsgo check fails the build while it is missing
([Deps Have to Be Direct](../rules/ts-compile.md#deps-have-to-be-direct)). One
in an npm package's declaration entry (`@types/bun/index.d.ts` is
`/// <reference types="bun-types" />`) is followed by the rule; see
[`@types/*` packages](../rules/ts-compile.md#types-packages).

## ts_test: needs vitest in the npm closure, which no dep provides

```
ts_test @@//path/to:my_test: @@//ts/runners:vitest runs the tests and needs
vitest in the npm closure, which no dep provides.
Add the hub label of each to deps.
```

The vitest runner runs `vitest` out of the importer chain the tests run in,
and a package no dep provides is not in it. List `@npm//:vitest` in `deps`:

```python
ts_test(
    name = "my_test",
    srcs = ["my.test.ts"],
    deps = [":my_lib", "@npm//:vitest"],
)
```

Every other package the run needs at runtime is in the runfiles the same way:
the test's npm deps' links and store trees, and each `ts_compile` dep's, so a
package only the production code imports arrives through that dep. See
[Runners](../rules/ts-test.md#runners) and
[Listing npm Deps](../rules/ts-test.md#listing-npm-deps).

## Isolated Declarations Error: Missing Return Type

Reported under `--//ts:declarations=oxc`, where Oxc derives `.d.ts` from
syntax, and in either mode under a `tsconfig.json` chain that sets
`isolatedDeclarations`: `TsgoCheck` keeps `declaration` on there, which the
option requires, and reports an unannotated export as `tsc -p` does:

```
src/gate.ts(2,10): error TS9013: Expression type can't be inferred with --isolatedDeclarations.
```

Under `--//ts:declarations=oxc` the emit, `TsEmit`, reports it too, in Oxc's
form:

```
× Isolated declarations error(s): TS9007: Function must have an explicit
│ return type annotation with --isolatedDeclarations.
```

Add the annotation the error names, or take `isolatedDeclarations` out of the
chain and build under the default `--//ts:declarations=tsgo`, where the
compiler infers it. See
[Isolated Declarations](../getting-started/isolated-declarations.md).

## Type Errors Are Not Failing the Build

`TsgoCheck` is a validation action on every target, in both
`--//ts:declarations` modes: Bazel runs it during `bazel build` on its own and
fails the build on a type error, unless `--norun_validations` turns
validations off; the `.bazelrc` line the quickstart writes requests the group
explicitly:

```
build --output_groups=+_validation
```

Under the default `--//ts:declarations=tsgo` a dependent's build fails a
second time in `TsgoDeclare`, whose `noEmitOnError` leaves no `.d.ts` behind.

## Gazelle Generating Wrong Deps

`deps` is what tsgo resolved when it listed the package's `tsconfig.json`, so
a wrong dep is a wrong resolution or a wrong label for the right file:

1. Run `bazel run //:gazelle -- -ts_verbose` and read the program's line: the
   files listed, tsgo's diagnostics, and the imports it could not resolve.
2. A missing npm dep is an import the checkout's `node_modules` does not
   answer (`pnpm install`), or a name the root `pnpm-lock.yaml` never mentions;
   the run names the importer and the specifier it refused.
3. A missing first-party dep is a file no package owns: the nearest
   `tsconfig.json` above it does not list it, or its directory is under a
   `# gazelle:exclude`; the run names the file and the programs that reached
   it. Put it in a program, or name its label with `# gazelle:resolve`.
4. `# keep` above a rule, or on a `deps` entry, holds what you wrote
   ([`# keep`](../gazelle/directives.md#keep)).

## Gazelle: .tsx file dropped for a same-named .ts

```
typescript: lib/a.test.tsx: in no program: lib/tsconfig.json lists lib/a.test.ts, the same module path, and the two would emit the same a.test.js
```

TypeScript expands `include` into one file per path without its extension and
keeps `.ts` over `.tsx`, so `a.test.tsx` beside `a.test.ts` is in no program and
in no target, and the two would compile to the same `a.test.js`. Gazelle fails
the run rather than leave the `.tsx` out. Rename one file of the pair
(`a.view.test.tsx`), or, when the `.tsx` is meant to stay out of every program,
exclude it with `# gazelle:exclude a.test.tsx` in its directory's BUILD file.

## ts_dev_server: has no node_modules attr

```
ts_dev_server: @@//src/app:dev has no node_modules attr, so the app's own
dependencies are not in runfiles.
Add node_modules = ":node_modules" pointing at a node_modules() target; the
generated config resolves every bare specifier through that tree.
```

For a hand-written `ts_dev_server`, name the importer's `node_modules`
target. Gazelle writes that target in the importer's package. Its `deps`
link every npm package the app imports; add `vite` when explicitly selecting
`@rules_typescript//vite:dev_server`:

```python
ts_dev_server(
    name = "dev",
    entry_point = ":app",
    node_modules = ":node_modules",
)
```

## ts_dev_server: sets react_refresh = True, but @vitejs/plugin-react did not load

This setting applies to the optional Vite server. The default oj server
provides native React Fast Refresh and rejects `react_refresh = True`.

The dev server does not start: it could not load the Fast Refresh plugin out of
the Bazel `node_modules` tree, and the message ends with the underlying cause.
Usually the package is not in the tree; add it to the `node_modules` target the
dev server uses:

```python
node_modules(
    name = "node_modules",
    deps = [
        "@npm//:vite",
        "@npm//:vitejs_plugin-react",
    ],
    hoist = ":node_modules/.pnpm/node_modules",
)
```

The target name matters too: the plugin resolves the `react-refresh` runtime by
Node's own walk-up, which only looks in directories called `node_modules`.

## [rules_typescript] Failed to load vite_config

`ts_dev_server` loads a copy of your `vite_config` from
`bazel-bin`, so the file's own imports resolve beside the Bazel npm tree, not in
your source tree. Staged there are the config and the modules `vite_config_srcs`
declares, nothing else:

- a relative import of a module not in `vite_config_srcs` fails, and the
  message names the file. Declare it:

  ```python
  vite_config = "vite.plugins.ts",
  vite_config_srcs = glob(["plugins/**/*.ts"]),
  ```

  A module outside the config's own Bazel package cannot be declared, since it
  would have to stage above the staging root; that is a separate analysis-time
  error naming the file and the package;
- a bare npm import works, as long as the `node_modules` target is in the same
  Bazel package as the dev server. Move it back beside the server, or add a
  `node_modules` target there.

See [`vite_config`: what it may import](dev-server.md#vite_config-what-it-may-import).

## [rules_typescript] ts_dev_server: the vite_config sets …, which the generated config does not read

The generated config reads `plugins` out of your `vite_config` and nothing else;
the dev server takes its serve root from the target. Every other key would be
silently discarded, so the load throws, naming the keys it found and the keys it
honours. A framework config that sets `define`, `resolve.alias`, `build.target`,
`optimizeDeps` or `root` hits this. Move what you need into a plugin. The check
runs at config-load time, not analysis time, because only the loaded object says
what keys it has.

## Dev server: Failed to resolve import "some-package"

The dev server resolves bare specifiers through the `node_modules` tree only, and
the package is not in that tree. Add it to the target's `deps` and restart:

```python
node_modules(
    name = "node_modules",
    deps = ["@npm//:vite", "@npm//:some-package"],
    hoist = ":node_modules/.pnpm/node_modules",
)
```

## invalid repository name '{$username}.tsx'

A source file whose name starts with `@` (a TanStack Start route on a dynamic
segment is written `@{$username}.tsx`) reached a `srcs` list bare. A `srcs`
entry is a label, and Bazel parses the head of the string before it looks at the
file system: `@` opens a repository name, `//` an absolute package, and a `:`
anywhere splits package from target. The bare name names a repository that does
not exist, and one such entry fails `bazel query //...` for every package in the
workspace, not for the one target.

Gazelle writes `":@{$username}.tsx"` for these, which pins the name to the
package the way a bare name does for every other file. Hand-written BUILD files
need the same leading colon.

A name holding a `:` has no label in any spelling, since a target name may not
contain one. Gazelle leaves such a file out of every target it generates, saying
so on one line per file. Rename the file.

`exports_files` takes the opposite spelling. Its argument is a list of target
names, not of labels, so the bare `exports_files(["@{$username}.tsx"])` is the
form there, and `exports_files([":@{$username}.tsx"])` fails with
`target names may not contain ':'`. A `filegroup` whose `srcs` is a `glob()`
needs neither spelling: a glob pattern is matched against the file system, not
parsed as a label.

## Snapshot 'x 1' mismatched, or a snapshot vitest says is new

`ts_test` runs vitest in read-only snapshot mode, so a mismatch is a failure and
a snapshot the sandbox cannot read counts as absent. Two causes:

- The `.snap` is not in `srcs`, so it never reached the runfiles tree. List it
  with the package's other files, as Gazelle does.
- The snapshot is stale. Regenerate it with `vitest -u` in the package, then
  commit.

Full workflow: [Snapshots](testing.md#snapshots).

## Slow First Build

The first build downloads a Rust toolchain, a tsgo npm tarball, a Node.js
tarball, and the npm packages your targets reach (not the whole lockfile), then
compiles `oxc-bazel` and its crate graph from Rust source. That compile takes
minutes. A build that registers
[tsgo from source](../rules/providers.md#tsgo-from-source) also fetches a Go
SDK and compiles tsgo from the pinned Go module. Everything after the first
build is cached; do not `bazel clean`.

Mount a persistent cache volume so CI pays it once:

```bash
docker run -v bazel-cache:/root/.cache/bazel my-image bazel build //...
```

## Container Builds

Bazel works inside Docker containers without privileged mode:

```dockerfile
FROM ubuntu:24.04

RUN apt-get update && apt-get install -y curl git \
    && rm -rf /var/lib/apt/lists/*

RUN curl -Lo /usr/local/bin/bazel \
    https://github.com/bazelbuild/bazelisk/releases/latest/download/bazelisk-linux-amd64 \
    && chmod +x /usr/local/bin/bazel

WORKDIR /workspace
COPY . .
RUN bazel build //...
```

Mount a cache volume, for the reason above. ARM64 containers work: `rules_rs`
builds `oxc-bazel` natively and the toolchain fetches the compiler's
`linux-arm64` platform package (`@typescript/typescript-linux-arm64` for a
`typescript` release).

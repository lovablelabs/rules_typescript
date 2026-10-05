# ts_compile

Publishes TypeScript sources and checks them with tsgo. With `emit = True`, Oxc emits JavaScript and tsgo emits the declarations a built-output consumer reads.

## Usage

```python
load("@rules_typescript//ts:defs.bzl", "ts_compile")

ts_compile(
    name = "lib",
    srcs = ["index.ts", "math.ts"],
    deps = ["//other/package", "@npm//:zod"],
    tsconfig = "tsconfig.json",
    visibility = ["//visibility:public"],
)
```

Unmodified TypeScript compiles: no explicit export type annotations, no extra
flags in `.bazelrc`. Every compiler option is the tsconfig's.

## Attributes

| Attribute | Type | Default | Description |
|-----------|------|---------|-------------|
| `srcs` | `label_list` | `[]` | The package's files: TypeScript is checked (and compiled with `emit = True`), JavaScript and declarations join the program, every other file is staged as data. May be omitted for a metadata-only publisher. See [Sources](#sources) |
| `type_inputs` | `label_list` | `[]` | Additional compiler inputs at their original paths, such as package scopes for borrowed declarations. These are not source roots, runtime files or outputs. |
| `package_scopes` | `label_list` | `[]` | Package.json metadata kept at its original compiler identity and placed relative to runtime modules within this compiler output namespace. A local package-root scope also supplies the npm publication manifest. These are not source roots or JSON module endpoints. |
| `data` | `label_list` | `[]` | Standalone asset files staged at package-relative paths in both emit modes. See [Package assets](#package-assets) |
| `emit` | `bool` | `False` | Publish TypeScript sources with validation. Set `True` only when a consumer needs emitted JavaScript or declarations. |
| `deps` | `label_list` | `[]` | `ts_compile`, `ts_codegen` or `ts_npm_package` targets, and a workspace member's link target `//<importer>:node_modules/<name>` |
| `tsconfig` | `label` | `None` | The project's own `tsconfig.json`, or a [`ts_config`](#ts_config) target: where every compiler option comes from. See [Where compiler options come from](#where-compiler-options-come-from) |
| `node_modules` | `label` | `None` | The `node_modules` target of the nearest lockfile importer at or above the package: the chain a direct npm dep resolves along. Required when the closure holds an npm package; Gazelle writes it. See [The node_modules Chain](#the-node_modules-chain) |
| `source_node_modules` | `label_list` | `[]` | Importer chains for retained sources, searched alongside `node_modules` for declared store Files. Nearest links and their types companions keep their original importer paths. |

Emission settings otherwise use build flags, one value for the whole build:

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--//ts:declarations` | `string` | `"tsgo"` | Which tool emits `.d.ts`: `"tsgo"` or `"oxc"`. See [Which tool emits the declarations](#which-tool-emits-the-declarations) |
| `--//ts:source_map` | `bool` | `True` | Emit a `.js.map` next to every `.js`. See [Source and declaration maps](#source-and-declaration-maps) |
| `--//ts:declaration_map` | `bool` | `False` | Emit a `.d.ts.map` next to every declaration. See [Source and declaration maps](#source-and-declaration-maps) |
| `--//ts:lib_check` | `bool` | `False` | Turn `skipLibCheck` off for every target. See [Finding a broken declaration](#finding-a-broken-declaration) |
| `--//ts:checkers` | `int` | `0` | The threads tsgo checks with: `--checkers N` on TsgoCheck and TsgoDeclare, and `cpu:N` on each so Bazel schedules it as N cpus. `0` leaves tsgo's own count, the four the checkout's `tsc -p` runs with. |
| `--//ts:lint` | `label` | `@lint_config//:lint` | The `lint_config` every target runs as its `TsLint` validation: the one the root module's `ts.lint()` writes. `//ts:no_lint` names no binary, so no target lints. See [Lint](../guides/lint.md) |

A target that has to be built under another value of one of these is reached
through a Starlark transition; `tests/flags.bzl` is the ruleset's own.

### Source-only programs

Source mode is the default for `ts_compile` and `ts_test`: TypeScript files reach source-native consumers, while tsgo checks remain in `_validation`. Vitest and OJ transform sources themselves, so an all-source closure needs no application JavaScript or declaration emit. Such a closure retains original paths. A source-mode consumer of a relocated dependency stages its unchanged TypeScript and data in the [shared source layout](#shared-source-layout); compiler and editor source identities remain original.

Set `emit = True` on programs consumed as built JavaScript or declarations. JavaScript-only `ts_binary` and Node-test consumers reject a source-only closure at analysis, naming the consumer, source-owning targets and the attribute to set, including workspace members. Gazelle derives opt-ins from Node binaries/tests and JavaScript/declaration package entries, following the resolved dependency graph. It does not infer requirements from whether build outputs happen to exist in the checkout. For a custom output consumer Gazelle cannot inspect, preserve an explicit `emit = True` with `# keep`.

Installed npm packages and optional server plugins retain their own formats. Source-mode package exports must name files present in the source package.

### Sources

`srcs` accepts every file. Source mode retains TypeScript as runtime inputs. The emission behavior below applies when `emit = True`; validation remains enabled in both modes. Four classes of src, by extension:

- **TypeScript**, `.ts` and `.tsx`: compiled to `.js` and `.js.map` -- by oxc,
  or by tsgo when the program's `module` is CommonJS-shaped ([The Module
  Format](#the-module-format)) -- type-checked by tsgo, and declared as `.d.ts`
  by whichever emitter `--//ts:declarations` names. A `.tsx` under
  `jsx: "preserve"` is compiled to `.jsx` and `.jsx.map`, the names tsc gives
  it, with its JSX left for the bundler ([below](#a-tsx-under-jsx-preserve)).
  A `.mts` or `.cts` is refused: the rule emits `.js` and `.d.ts` from `.ts`
  alone, and has no output shape for one.
- **JavaScript**, `.js`, `.mjs` and `.cjs`: copied when staged into the output tree so their relative imports resolve beside emitted modules after Node resolves symlinks. Unrelocated source mode retains the original JavaScript and imported JSON Files; dependency publications keep their selected Files. Explicit JavaScript assets remain staged in both modes. Source-capable test runners place JavaScript beside the original TypeScript inputs at their repository paths. JavaScript also joins the type program. The rule sets `allowJs` for it, so its
  JSDoc types reach consumers; `checkJs` in the tsconfig has its own body
  checked. A `.jsx` is rejected at analysis time: JavaScript is staged
  unchanged, and tsc would transform the JSX in one under every `jsx` mode but
  `preserve`; the message says to rename it `.tsx`.
- **Declarations**, `.d.ts`, `.d.mts` and `.d.cts`: in the type program and
  passed through to consumers unchanged, global when the file has no top-level
  import or export. A `.d.mts` or `.d.cts` is the declaration of the `.mjs` or
  `.cjs` of the same stem, the pairing `tsc` resolves by name, so
  `import { compile } from "./compile.mjs"` resolves to `compile.d.mts` ahead
  of `compile.mjs`, and a checked-in declaration types an untyped JavaScript
  module whether or not that module is in `srcs`. When it is, the `.mjs` is
  staged and leaves the type program: TypeScript keeps the higher-priority
  extension of a pair listed together, as `tsc` does. The checked-in file is
  then the module's only declaration, and `checkJs` does not reach that `.mjs`.
- **Data**, every other file: staged into the output tree unchanged at its
  package-relative path, so the compiled module beside it reaches it by the
  same relative path at run time -- `import data from "./data.json"`,
  `import "./styles.css"`, `new URL("./logo.svg", import.meta.url)`, a
  `readFileSync` of a fixture. A consumer gets the closure as
  `TsInfo.transitive_data`; `ts_test` stages it in the runfiles beside
  the `.js`, `ts_binary` in its runfiles and its bundle, `ts_dev_server` in
  its runfiles. What the import of a data file is typed as is the tsconfig's
  to say: `vite/client` in `types`, or a `declare module "*.svg"` in a
  declaration src. A data file is never a tsgo input, with one class of
  exception: a `.json` is, this target's and its deps' alike. An import of it
  resolves to the file and is typed from its contents under
  `resolveJsonModule`, which bundler resolution implies. JSON imported from a
  package without a TypeScript owner is a source-file label in `srcs`; it
  names the original package, which controls its visibility.
  A foreign JSON input follows its importing module's runtime layout. Source
  mode retains original paths unless a relocated dependency requires the
  [shared source layout](#shared-source-layout). Use `data` for a foreign
  JSON file packaged as a standalone asset.
  Gazelle derives imported JSON labels from the compiler listing. A
  `package.json` read only for package format or self-reference resolution
  belongs in `package_scopes`; a JSON import of that same file keeps it in
  `srcs`, with its authored contents and module layout.

Gazelle writes the first three classes from tsgo's listing of the package's
`tsconfig.json` -- a file whose extension tsgo could have listed is a src only
when the program lists it, the JavaScript twin of an owned declaration apart --
and the fourth from the package's tree: every other regular file under it that
no deeper package, `out_dir` or BUILD file claims, with package metadata
declared through `package_scopes`
([the package model](../gazelle/overview.md#the-package-model)).

### Compiler inputs and emitted layout

`type_inputs` supplies files the compiler reads without making them roots or runtime assets. Gazelle uses it for the package scope of a borrowed declaration; the declaration stays in `srcs`. Dependency consumers retain these inputs, including a `ts_test` that checks a dependency from sources under the same tsconfig. A File also listed in `srcs` keeps its source and runtime role. An explicitly imported JSON module belongs in `srcs`, even if the compiler also reads it as metadata.

`package_scopes` supplies package.json metadata needed by runtime sources. The compiler receives the original File; the runtime scope follows the module's published path. Unchanged source-mode placement retains the original scope File, including for generated modules whose physical files live in the build output tree. Local package targets that name declared modules follow their published filenames, so an emitted `value.ts` becomes `value.js`. Source-mode filenames retain their source extensions; native JavaScript, declaration conditions and the authored package format are preserved. Local scopes and coherent foreign trees can be staged within the compiler's output namespace. A dependency can supply the same scope, including through a scope-only forwarder. A local scope with no module placement is published at its package-relative output path so an emitted descendant can use it; it keeps its authored targets unless the publisher holds their module mappings. An explicit JSON import puts the File in `srcs`, which takes precedence and retains its authored contents and the runtime consumer's module layout checks. A scope needed only by borrowed declarations remains in `type_inputs`.

The local package-root `package.json`, declared in either `package_scopes` or `srcs`, also supplies the existing npm publication manifest, `<name>.package.json`. With emission enabled, `tsaction manifest` rewrites source entry points to JavaScript and type entry points to declarations, and supplies `type: "module"` when absent. A dependent's compiler overlay and a workspace member's npm store use that publication manifest. Runtime scope projection preserves authored format and declaration conditions because it serves the files beside the runtime modules. Source mode uses the original manifest unless dependency relocation requires a projected manifest naming the staged sources. Borrowed and nested scopes do not become package-root publication manifests. See [What a Workspace Member Is Imported As](../guides/npm.md#what-a-workspace-member-is-imported-as).

Each shared runtime scope has exactly one publisher, including across disjoint programs. A scope also listed in `data` uses that same projection. A dependency's recorded scope output is reused when its targets and package format agree with the declared runtime modules. Equal runfiles paths coalesce once; different destinations retain both entries. A different source at the required destination fails analysis. Older custom providers without scope provenance still pass through ordinary data, but cannot supply an occupied destination whose source identity is unproven.

For disjoint emitted leaves `:a` and `:b` in one Bazel package, give each leaf the authored metadata through `type_inputs = ["package.json"]`. One `ts_compile` aggregate in that package declares `srcs = []`, `package_scopes = ["package.json"]` and `deps = [":a", ":b"]`. It projects the shared scope from both leaves' module mappings. A consumer depends on this aggregate and each leaf it directly imports, preserving strict deps; a binary uses the importing program as its entry point. The aggregate supplies the runtime scope through `deps` without making either leaf a second publisher.

Projection covers declared module targets, including matching wildcards; unrelated targets stay authored. Explicit entries can mix emitted and source-native modules. A wildcard that would need different filenames for its declared matches, or an incompatible reused scope, fails with the conflicting targets. Reused scopes are checked before TypeScript emission; JavaScript-only and source-mode programs use their existing Bazel validation action, which does not order independent runtime actions.

A scope above the compiler's output namespace stays at its original location. For `app/main.ts` under a root manifest, the emitted counterpart would be `bin/package.json`, outside `//app`'s outputs. The rule does not create `bin/app/package.json`: that closer manifest would change relative package-import targets. A binary's staged runfiles can retain the original root manifest at the correct ancestor location. Its targets still need to agree with the published runtime modules.

An emitted program places its modules, imported JSON and runtime scopes under one logical source root in the target's output namespace. Generated files use their source-relative coordinates without the `bazel-out` prefix; external repositories retain distinct coordinates. The producer records exact source/runtime and source/declaration File pairs. A consumer whose dependencies have moved extends that layout and links their canonical outputs at the paths its relative imports require. Unbundled binaries and tests use the published runtime Files. Source-mode consumers also follow relocated dependencies while retaining TypeScript bytes; an all-source closure keeps original paths. See [Shared Source Layout](#shared-source-layout) for output paths and declaration emission.

### Package assets

`data` packages standalone files at the target's package-relative paths. For
example, `data = ["//:metadata.json"]` on `//packages/shared:shared` stages
`packages/shared/metadata.json`; a workspace package exposes it as
`node_modules/shared/metadata.json`. Files from a foreign subdirectory retain
that subdirectory below the target's package. Generated files already at the
destination keep their existing producer. List individual files; a generated
tree belongs in `deps`.

Keep imported JSON modules in `srcs` and metadata-only package manifests in
`package_scopes`. Moving a foreign standalone JSON asset from `srcs` to `data`
is required to retain its
package-local placement in source mode. Existing non-JSON assets in `srcs`
remain supported. Gazelle preserves hand-written `data`; it does not infer
asset intent from an import or a filename. Files also listed in `srcs` keep
their source behavior; `data` stages no extra copy.

Assets travel through `TsInfo.data` and `TsInfo.transitive_data`, so tests,
binaries, dev servers and workspace packages receive the same files. This
attribute packages files; [`ts_test.data`](ts-test.md#attributes) supplies
extra test runfiles at their existing paths.

When module or declaration placement already requires a common layout, declared asset coordinates extend that same namespace. A moved consumer reads ordinary assets from Files in that layout, which copy the original bytes so relative references keep their published context. Executable module aliases retain links to their canonical runtime Files. Assets alone do not move a program or establish module ownership. The optional [`asset_files` record](providers.md#tsinfo) preserves the original File, package-local coordinate and published File without guessing origins from output names.

### Source and Declaration Maps

Turn `--//ts:source_map` off for a build whose JavaScript nothing debugs, or
whose bundler makes its own map.

`--//ts:declaration_map` is what makes go-to-definition across a package boundary
land on the `.ts` source. It requires the tsgo declaration emit; under
`--//ts:declarations=oxc` it is an analysis error naming both flags, since oxc
emits no map.

A `.js.map` has one shape from either emitter: `sources` names the src by its
exec-root-relative path -- `tests/smoke/hello.ts`, the path Bazel and a
coverage report use -- and `sourcesContent` carries the text, so a consumer
resolves nothing against the map's directory. oxc writes that form. tsgo
writes its maps into `TsEmit`'s scratch `outDir`, `sources` relative to the
map as tsc computes them and `sourcesContent` from `--inlineSources`, and the
move into place resolves each `sources` entry to the exec-root path
([The Module Format](#the-module-format)).
`//tests/compiler_options/source_maps` pins both emitters' maps.

## Outputs

With `emit = True`, for each source file `foo.ts`:

| Output | Description |
|--------|-------------|
| `foo.js` | Compiled JavaScript ([The Module Format](#the-module-format)) |
| `foo.js.map` | Source map, under `--//ts:source_map` ([Source and Declaration Maps](#source-and-declaration-maps)) |
| `foo.d.ts` | Declaration file, the compilation boundary: `TsInfo.declarations` and the `declarations` output group, not a default output ([Which Tool Emits the Declarations](#which-tool-emits-the-declarations)) |

For a `foo.tsx` under `jsx: "preserve"` the first two are `foo.jsx` and
`foo.jsx.map`, as tsc names them ([below](#a-tsx-under-jsx-preserve)). A
program tsgo emits has one more, `<name>.es/foo.js`: the ES module a vitest
test runs in place of `foo.js` ([The Module Format](#the-module-format)),
in no default output. `bazel build //pkg:lib` writes the `.js` and runs the
check; the `.d.ts` is written when a dependent's compile reads it, or with
`bazel build //pkg:lib --output_groups=declarations`. Every other src is staged at its package-relative path,
unchanged; a `package.json` at the package's root also gets
`<name>.package.json`, the manifest as built ([Sources](#sources)).

## Where Compiler Options Come From

The tsconfig the actions read is written by `tsaction tsconfig`, one `TsConfig`
action per target. It `extends` two files, the ruleset's baseline and then the
target's `tsconfig`, runs `tsgo --showConfig` over that chain to read the
effective options and the roots the chain names, and writes the resolved
options with Bazel's existing overrides into one config without `extends`.
The pass that runs `--showConfig` reads the chain's own roots;
a chain naming neither `include` nor `files` gets tsc's default `**/*` over
the tsconfig's directory, so tsc walks the project and not the output
directory the written file sits in, and a JavaScript src puts `allowJs` on
that pass too. Lowest precedence first:

1. **The ruleset baseline**: `strict`, `module: "Preserve"`, `target: "es2022"`,
   `jsx: "react-jsx"`, `skipLibCheck`, `esModuleInterop`. Without a `tsconfig`
   they are the program's whole options; with one they reach only the keys the
   file and its own `extends` chain never mention.

   `moduleResolution` is asserted nowhere: TypeScript couples it to `module`, and
   tsgo derives the resolver from whichever `module` wins, `Bundler` for all of
   them but `Node16`/`NodeNext`.
2. **`tsconfig`**: the project's own `tsconfig.json`, and whatever it extends.
   Referenced where it lives, never copied, so relative paths inside it resolve
   against the directory they were written for. Everything it says wins over
   layer 1, so tsgo checks the code under the options `tsc` would.
3. **The keys Bazel owns**, written last: `rootDirs` bridging the source and
   output trees, `rootDir` (the exec root, which every input is under),
   `composite` and `incremental` off, and the declaration emit off --
   `declaration`, `declarationMap`, `emitDeclarationOnly` false,
   `declarationDir` null -- since a chain sets those under a `composite` this
   file turns off. `declaration` stays true where `isolatedDeclarations` is
   in force, which requires it (TS5069): a chain that sets the option, or
   `--//ts:declarations=oxc`, which writes it; `allowJs` when a src is
   JavaScript; `skipLibCheck: false` under `--//ts:lib_check`. No emit shape:
   each tsgo run says on its command line what it emits
   ([Architecture](#architecture)).
   The roots are the tsconfig's own: `files`, `include` and `exclude` are the
   chain's specs, each rewritten from the directory of the chain file that
   set it, a chain naming neither `files` nor `include` getting tsc's default
   `**/*` over its directory; `include` then names each src no root of the
   chain names, and each path-shaped `types` entry as a root file at the
   path it is staged (below), one entry per path, so tsc's rules for a
   pattern's match -- `allowJs`, the higher-priority extension of a pair
   listed together -- reach a src named by path, as they reach nothing in
   `files`. The chain's `exclude` is such a rule too: a src it names is in
   the program only through an import, and one the program never read fails
   `TsgoCheck` naming the src and the entry -- the program is the srcs, so
   the file leaves `srcs` or the entry leaves `exclude`. A chain that sets
   no `exclude` gets `[]`,
   not tsc's default list, whose `outDir` entry is the output directory such
   an entry sits in. `references` is `[]`. The roots are patterns rather than
   a `files` entry per src because `--explainFiles` finds a `files` root's
   reason by a scan of that list, once per root. Two keys are the tsconfig's
   values rewritten: `paths`, each value from the directory of the chain file
   that set it and a `bazel-bin` twin beside it, and `types`, its package
   names alone. A value the tsconfig sets for one of these keys is overridden
   by `extends` order, not refused.

`--showConfig` is run over the chain, not over the user's file alone, so a
default the baseline supplies reaches oxc as tsgo sees it: oxc transforms with
the `target`, `jsx` and `jsxImportSource` the same run yields, handed over in
`<name>.options.json`, and the two compilers agree. The same file carries the
chain's `module`, which decides which tool emits the JavaScript
([The Module Format](#the-module-format)).

The program root keeps a program to its declared inputs
([The node_modules Chain](#the-node_modules-chain)): it holds the action's
source inputs and nothing else of the source tree, and of a dep laid over the
package its declarations and data and none of its JavaScript, so a pattern
names the target's srcs and the deps' declarations whether or not the action
is sandboxed. tsgo reads a root file,
a relative import and a `paths` match at the path given, and resolves a bare
specifier and a type reference directive to their realpaths. A path-shaped
`types` entry is therefore listed as a root file: read at its path under the
root, its own imports resolve among the declared inputs and nothing the
source tree has beside them -- an import inside a staged `.d.ts` that names a
file nothing stages resolves to nothing, and `skipLibCheck` drops the
`TS2307`. A package's file resolves at its realpath, where its own imports
are.

Read the tsconfig a target handed the compiler with
`bazel build //pkg:lib --output_groups=tsconfig`.

### A `.tsx` Under `jsx: preserve`

tsc names a `.tsx`'s emit `foo.jsx` under `jsx: "preserve"` and `foo.js` under
every other mode, and leaves the JSX in it for the bundler; a `.ts` is `foo.js`
under every mode. The rule names its outputs at analysis, before any action has
read the tsconfig, so the one compiler option that names an output is declared
on the tsconfig's [`ts_config`](#ts_config), `jsx = "preserve"`. `ts_compile`
and `ts_test` read it through `TsConfigInfo`; Gazelle writes it from the `extends` chain the way it writes `deps`; and the
`TsConfig` action, which runs `--showConfig` over the chain, fails a target
with a `.tsx` src when the declaration and the file disagree:

```
tsaction: pkg/tsconfig.json: jsx is "preserve", so a .tsx emits .jsx, and the
rule declared .js: declare it on the tsconfig's ts_config, jsx = "preserve"
```

A tsconfig passed as a plain file declares nothing, so one that sets `preserve`
fails the same way, for a target with a `.tsx` src, without a `ts_config`;
a `.ts`-only program has nothing `jsx` names. From the declaration on, the
emit is tsc's: oxc names the file `.jsx` when it transforms under
`--jsx preserve`, every consumer stages a `.jsx` as it stages a `.js`, a
`ts_test` runs a `.jsx` test file and resolves a `.tsx`
setup file to it, and the target writes its `package.json` as built with a
`.tsx` target rewritten to the `.jsx`, the manifest the hub's view of a member
links beside the `.jsx` at its package-relative path. Vite transforms a `.jsx`
module and refuses JSX in
a `.js` (`Failed to parse source for import analysis ... If you are using JSX,
make sure to name the file with the .jsx or .tsx extension`), which is what a
`.tsx` compiled to `.js` under `preserve` met. `//tests/jsx_preserve` is the
example: a `.tsx` test file, a `ts_compile` consumer, and a runtime the vitest
config aliases the way `node_modules` would hold a published one;
`//tests/jsx_preserve/member` is a workspace member under it whose `exports` is
`./view.tsx`, imported by name through the view.

The alternative was an emit declared as one directory whose contents tsaction
names after reading the tsconfig. Rejected: a directory's children cannot be
named at analysis, and every consumer names them -- `import "./foo"` resolving
to `bazel-bin/.../foo.js`, the runfiles a `ts_test` stages by path, the member
view's manifest targets, a `ts_binary`'s entry, the `paths` bin-dir twins -- so
the whole per-file output model would have moved into trees for one extension.

### The Module Format

The JavaScript a target emits has the module format tsc gives the program:
`module` in the tsconfig chain and, under `node16`, `node18` and `nodenext`,
the nearest `package.json`'s `type` per file, as tsc reads them. oxc's
transform keeps the module syntax it reads, which is the emit of every ES kind
(`es2015` through `esnext`) and of `preserve`, so those programs are oxc's.
Every other kind -- `commonjs`, the `node*` kinds, `amd`, `umd`, `system` --
is tsgo's: `TsEmit` runs `tsgo --noCheck` from the program root the check
runs in and moves each src's `.js`, `.js.map` and, under
`--//ts:declarations=oxc`, `.d.ts` into place, the map's `sources` resolved
to exec-root paths on the way
([Source and Declaration Maps](#source-and-declaration-maps)). `--noCheck`
emits from the parsed program with tsc's import elision and reports no type
error; the type check stays the tsgo action's, in either mode.

The default source-built tools share the ruleset revision. Custom action helpers and prebuilt archives must implement the matching action arguments. Optional provider fields let new consumers read older records; they do not make older tools accept new command arguments or launcher configuration.

oxc lowers what the chain's `target` lacks and keeps a top-level `await`
wherever tsc keeps one: `module` `es2022`, `esnext`, `system`, `preserve` or a
`node*` kind, under a `target` of `es2017` or later (TS1378 otherwise). tsc
never lowers it, and oxc's only handling of one below `es2022` is a refusal,
which `tsaction` turns off through oxc-bazel's `--top-level-await` under that
rule; the rest of the lowering follows the `target`.
`//tests/compiler_options/top_level_await` pins it under `target: es2020`.

The emit's inputs are the check's for every program -- the srcs, the chain's
links and store files, the tsconfig chain, the deps' declarations -- since
which tool emits is decided when the action runs, so an oxc emit waits for
the store and re-runs when a linked package changes. A CommonJS-shaped program
builds the tsgo program for the emit under `--noCheck` and again for the
check, and under the tsgo emit a third time in `TsgoDeclare` when a dependent
reads its declarations.

A CommonJS program's compiled code has `require`, `exports`, `__dirname` and
`__filename`, and a named import from a CommonJS dependency is that
dependency's export -- `import { app } from "electron"` -- where ES-module
linking sees only what `cjs-module-lexer` finds. Under
`--//ts:declarations=oxc` such a program's declarations are tsgo's
isolated-declarations emit, under oxc's rule: every export annotated. CommonJS
and declaration emission use the same [source layout](#shared-source-layout),
including programs with authored and generated inputs.
`//tests/node_test/cjs` pins the format at run time.

The format is the tsconfig's, not the manifest's alone: a package whose
`package.json` has no `type` and whose tsconfig says `module: "ESNext"` emits
ES modules, which node runs as such. A package that runs as CommonJS says so
where tsc reads it, `module: "commonjs"` or `"nodenext"`.

A vitest test runs ES modules whatever the program's `module`
([Runners](ts-test.md#runners)): vitest imports every file through vite's
transform, and the checkout never runs tsc's output under it, so `module`
describes the package's published output and not its tests -- and vitest
refuses a CommonJS importer outright (`Vitest cannot be imported in a CommonJS
module using require()`). A `ts_test` under the vitest runner therefore emits
its own srcs as ES modules (`TsEmit -es_modules`: oxc's transform with the
chain's `target` and `jsx`, reading the srcs and the options alone), and a
`ts_compile` whose program tsgo emits also emits the ES twin of each `.js`:
`foo.js` and `<name>.es/foo.js`, oxc's transform of the same source in a
second `TsEmit`. A vitest test that depends on it stages the twin at the
`.js`'s runfiles path, so `import "./foo"` and a `setupFiles` entry reach the
ES module; every other consumer -- node:test, `ts_binary`, a member's hub view
-- runs the `.js`. The twins travel in `TsInfo.transitive_es_twins`
([providers](providers.md#tsinfo)), a runner asks for them with
`TsTestRunnerInfo.es_modules`, and they are in no default output. The
alternative, a configuration transition on a vitest test's deps, would build
every program a vitest test reaches a second time; a twin is one oxc run over
a program tsgo emits, and nothing else changes.

The rule names its outputs before any action reads the tsconfig, so, as with
`jsx`, the `module` that names the twins is declared on the tsconfig's
[`ts_config`](#ts_config): `module = "commonjs"`, `"node16"`, `"node18"` or
`"nodenext"`, lowercased as tsgo prints it, and unset for an ES kind or
`preserve`. Gazelle writes it from the `extends` chain, and the `TsConfig`
action fails a target with a `.ts` src when the declaration and the chain's
effective `module` disagree:

```
tsaction: pkg/tsconfig.json: module is "commonjs", so tsgo emits the
JavaScript and a vitest test runs its ES twins, and the rule declared none:
declare it on the tsconfig's ts_config, module = "commonjs"
```

A tsconfig passed as a plain file declares nothing, so a CommonJS-shaped one
fails the same way without a `ts_config`. `//tests/vitest/commonjs` is the
example: a `module: commonjs` package with no `type`, whose setup file imports
vitest from the `ts_compile` the test depends on.

### Comments

The compiled JavaScript keeps the comments the source has above its
statements. Under oxc, a comment above a top-level statement the transform
erases -- an `import type`, an import whose every binding the file uses as a
type, an `interface`, a `type` alias -- moves to the next statement the source
keeps, or to the file's end when none follows, so a file that opens with
`// @vitest-environment node` over an `import type` keeps the docblock above
its first remaining import; vitest reads it from the file it runs, under Bazel
the compiled sibling. tsgo's emit drops such a comment.
`//tests/vitest/docblock` pins it.

### What Fails Before tsgo Runs

Analysis rejects a `.jsx` src, a directory in `srcs` (a `ts_codegen` `out_dir`
tree belongs in `deps`), a `.mts` or `.cts` src,
`--//ts:declaration_map` under `--//ts:declarations=oxc`, and a target with
sources and no tsgo toolchain. `tsaction`
fails the `TsConfig` action on a path-shaped `types` entry no input sits at
([a `types` entry that names a declaration file](#a-types-entry-that-names-a-declaration-file)),
naming the entry, the tsconfig and the path it looked for, and on a `jsx` or
`module` declaration the chain's effective value contradicts
([above](#a-tsx-under-jsx-preserve); [The Module Format](#the-module-format)),
naming the edit.

### Shared Source Layout

An emitted target keeps paths relative to the common logical root of its package and source files. For `//app:app` with `app/index.ts` importing `../foreign/value.js` from `foreign/value.ts`, the outputs are `bazel-bin/app/app/index.js` and `bazel-bin/app/foreign/value.js`, with declarations beside them. Generated sources participate at the same logical paths as authored sources; their physical `bazel-out` prefix does not change that relationship. A package with only local sources and dependencies at their original logical locations keeps its existing package-relative outputs.

A dependent that imports a relocated producer uses the same source coordinates for its own outputs and dependency links. Each link targets the dependency's canonical File; the consumer does not recompile it. Runtime JSON and package scopes follow the modules. Declarations retain exact source origins through `owner.declaration_files`, so a consumer's compiler overlay resolves the original import to the published declaration even when its output path moved.

A declaration overlay cannot replace an existing declaration with a different canonical target. Program-root staging rejects that overlap before running the compiler; repeated links to the same target coalesce. Two targets that independently emit a borrowed source can therefore be valid separately but unsupported in one consumer. Publish the shared module once and depend on that producer, or keep the emitted closures in separate consumer programs when their configurations must differ. This boundary also applies to source-mode consumers of emitted declarations.

The same layout applies when a source-mode consumer reaches a relocated dependency. Its own TypeScript and data are staged without emission, and dependency links target canonical Files. Compiler and editor inputs keep their original identities; runtime entry selection and snapshot paths use the producer's exact source/runtime pairs. An all-source closure without relocated dependencies retains its original paths.

A workspace member's npm store copies files into its package tree. It rejects selected canonical module and declaration links because copying them would lose the dependency's package scope and npm importer. Publish that dependency separately through npm or include its sources in the member. A member that owns its borrowed sources can publish its self-contained projected outputs; files still published outside the member directory remain unsupported.

`TsgoDeclare` uses the shared emitter with `-declarations_only`. It checks the full tsgo program, emits into a scratch directory under one logical compiler `rootDir`, and projects each declaration to the output declared for its source. Authored and generated inputs can therefore share a checked declaration action. CommonJS emission uses the same placement step for JavaScript. A `ts_test` uses the shared layout but publishes no declarations ([The Test's Program](ts-test.md#the-tests-program)).

## Deps Have to Be Direct

A source may import only what a **direct** dep provides. `TsgoCheck`, the
validation every program runs, runs with `--explainFiles`, and tsaction reads
the listing it prints, every
file in the program with the edge that brought it in, against an ownership
manifest the rule writes beside it: the target's own srcs, each first-party
target in the closure with the files it stages (`TsInfo.owners`), and the
closure's packages by store tree -- the tree each link of `deps` enters, and
every other tree the closure holds with the hub label to add. An edge from one
of the target's own files into a file whose owner is not in `deps` fails the
action, naming the label:

```
ERROR: .../src/app/BUILD.bazel:3:11: TsgoCheck //src/app:app failed: (Exit 1)
tsaction: //src/app:app imports files no direct dep provides:
  src/app/main.ts imports "./hidden"
    resolved to bazel-out/k8-fastbuild/bin/src/app/hidden.d.ts
    add //src/app:hidden to deps
  src/app/main.ts imports "zod"
    resolved to .../node_modules/.pnpm/zod@3.24.2/node_modules/zod/index.d.ts
    add @npm//:zod to deps
Each reaches this target only through another dep's own deps. Run Gazelle,
which writes deps from these edges, or add the labels above by hand.
```

`bazel run //:gazelle` writes those labels from the same listing. There is no
flag and no opt-out. The npm label in the message is the hub's root view,
`@<hub>//:<name>`; Gazelle spells a name under the nearest importer on the
chain that declares it, `@npm//web:zod`, the resolution the importing file's
walk up meets first
([The Lockfile Gate](../gazelle/overview.md#the-lockfile-gate)).

**What is checked:** every `Imported via`, `Augmented via`, `Referenced via`
and `Type library referenced via` edge whose importer is one of the target's
own files -- a type-only import, a `paths` alias, an `import()` type, a
`declare module` augmentation, a `/// <reference path>`, a `/// <reference
types>` and the JSX runtime import tsgo adds to every `.tsx` alike, since tsgo
resolved each one and says which file it landed in. A
file under the store is its tree's: tsgo lists the realpath of a file a bare
specifier reached, `node_modules/.pnpm/<key>/node_modules/<name>/...`, and
lists a file a relative specifier reached through a link,
`../node_modules/<name>/dist/internal.js`, at the link, which tsaction reads
to the same tree. The manifest carries
the key of the tree each link of `deps` enters, so an npm alias -- a link name
at the aliased package's tree, `tailwindcss-v3` at `tailwindcss@3.4.18_...` --
is declared by the name the importer links. A direct package's `@types/<name>`
twin, which an importer on the chain links beside it, counts as declared. An
edge into the target's own srcs passes. An import of a package's
`package.json` lands on the manifest as built the program root lays at the
package's path, and the compile that wrote it owns it as it owns the src
([The node_modules Chain](#the-node_modules-chain)).

**What is exempt:** an edge from a dep's own file -- its imports are that dep's
to declare, and a `/// <reference types>` the chain answers is written from the
listing
([Import Resolution](../gazelle/overview.md#import-resolution)); a tsconfig
`types` entry, which is an entry rather than an edge; the
toolchain's `lib.*.d.ts`; and a specifier tsgo could not resolve, which has no
file to own and is `TS2307`. A target with no program -- declarations alone in
`srcs` -- runs no tsgo action, and so has no edges to check.

### The node_modules Chain

npm packages reach tsgo the way they reach node: through the `node_modules`
directories above the importing file. `node_modules` names the nearest
lockfile importer's target at or above the package. Each chain searches its
`parent`s nearest first, pnpm's walk-up
([The Chain](node-modules.md#the-chain)). A direct npm dependency's store File
must match the nearest link on at least one chain: `node_modules` or a retained
source's `source_node_modules`. An absent name or a store File that no context
supplies fails analysis with the importer or dependency to correct.

Different importer locations can supply different versions of the same name in one program. Different store Files at the same actual link path are an error. Registry packages and workspace member links use the same File identity check; matching package/version keys alone do not establish identity. At each importer, a concrete package link takes precedence over a workspace-member fallback. Multiple labels for the same store File are accepted when a declared chain supplies it. First-party dependencies retain their npm stores and importer contexts for declaration checking. Runtime package selection follows the runner's rules, including the [node:test consumer-chain limit](ts-test.md#the-nodetest-runner). The action stages each context's nearest links for the direct names,
the `@types/<name>` twin an importer links beside one, the member links `deps`
name, the store trees and edge links their closures hold, the lockfile's hoist
links whose names the closure holds with the trees they enter
([The Store](node-modules.md#the-store)), and every
first-party dep's (`TsInfo.npm_files`): a dep's emitted `.d.ts` imports the
packages the dep declared, and they resolve from the dep's own importer's
links, an ancestor of its declarations under `bazel-out`.

Retained inputs in another importer use `source_node_modules` to keep that importer's lookup directory. Each context's nearest links and their own `@types` companions stay in place, including links that do not match a declared dependency, so another context's ancestor cannot replace a nearer binding. Only matching package bindings and their companions count as direct dependencies for the ownership check. Distinct companion Files retain their own importer paths. A matching binding on a retained source's chain is accepted even when the consumer's chain selects a different store File. The source owner's provider carries these importer directories downstream. An importer a dep or retained source brings from outside the chain resolves its own links first, then the chain's importers below the lockfile root in chain order, then the root.

tsgo walks up from the importing file for a bare specifier, so `tsaction tsgo`
lays out a program root under the target's output directory -- declared
compiler inputs linked at logical source paths to their exact Files under
real directories, the output tree
`bazel-out` linked whole, each importer's `node_modules` at the importer's
directory, the lockfile's root importer's at the root's `node_modules`, and
the declarations, the data and the `package.json` as built of every
first-party dep whose package is at or above the target's laid over that
package's directory, the declarations beside the sources and the manifest at
the package's path -- and runs tsgo from there. A dep's JavaScript is never
laid: a program reads a dep through its declarations, and under `allowJs` a
`.js` under the tsconfig's pattern would be a root beside its `.d.ts`, one
the declaration emit writes a `.d.ts` for onto the dep's own.
Generated roots use source-relative coordinates to resolve original package
scopes and helpers; this compiler view does not change runtime publication.
The root holds the srcs, the tsconfig chain, the deps' checked-in declarations
and, on a `ts_test`, the sources of the deps under its tsconfig ([The Test's
Program](ts-test.md#the-tests-program)), so the tsconfig's `include` names the
target's srcs and those sources and, where a dep is laid over the package,
its declarations under the patterns; a dep's emitted declarations and the
store are reached by import.
A bare specifier, an `exports`
condition, a subpath, a `@types/*` pairing, a `types` entry and the package's
own name through the nearest manifest resolve as tsc resolves them over a pnpm
install, and a declaration tsgo emits names a package the way that package's
`exports` allow. A file the root links is listed by its path under the root,
which the ownership check reads through the link: to the dep's output, or
to the store tree a chain link enters. A
package's own imports resolve from its realpath in the store,
`node_modules/.pnpm/<key>/node_modules/<name>/`, to the edges beside its tree,
so every dependent reaches the resolution pnpm recorded for it, and an import
of a name the package does not declare to the hoist's link at
`node_modules/.pnpm/node_modules/<name>`, as in the checkout. npm deps
contribute no other input: the declarations a program reads are its
first-party closure's, the `TsInfo.owners` records', and a package's file sits
under `node_modules/<name>/`, the segment TypeScript reads to take it for a
library file, type-checked and never emitted.

A workspace member is one of those packages. Its importer's link target,
`//<importer>:node_modules/<name>`, enters the member's store tree, which
holds the member's `package.json` as built -- every source-file target under
`main`, `module`, `browser`, `exports` and `imports` rewritten to the emitted
`.js` (the `.jsx` for a `.tsx` under `jsx: preserve`), `types` to the `.d.ts`
-- beside the member's `.js` and `.d.ts` at the paths the manifest names, so
the bare name and each `exports` subpath resolve for tsgo and for node through
one manifest. See
[what a workspace member is imported as](../guides/npm.md#what-a-workspace-member-is-imported-as).

### `@types/*` Packages

DefinitelyTyped publishes `x`'s declarations as `@types/x`, and a scoped
`@a/b`'s as `@types/a__b`. The hub pairs the two from the lockfile, and a
`ts_npm_package` carries its paired `@types/*` package in `transitive_deps`, an
importer that declares both links `@types/x` beside `x`, and tsgo pairs them
by walking `node_modules/@types`, as it does over an install. Which of the two a name
resolves to follows npm: `x` is answered by the runtime package when it
publishes declarations of its own, and by `@types/x` when it publishes none. A
`@types/*` entry that forwards (`@types/bun/index.d.ts` is exactly
`/// <reference types="bun-types" />`) resolves the directive through the same
walk.

`types` is written from the chain's entries. When the chain sets neither
`types` nor `typeRoots`, the direct `@types/*` deps' names are written, so
nothing auto-includes: a `@types/*` package the closure carries but no entry
names is in the store for the imports that reach it and out of the global
scope, and a use of its globals is `TS2304`.
`//tests/npm:transitive_types_probe` pins that. A chain that sets `typeRoots`
and no `types` gets no `types` key: those roots bound automatic inclusion, as
in the checkout, and tsgo skips the `node_modules` walk for a `types` name
under a custom `typeRoots`, so a deps' name written there is `TS2688`; a store
file's own `/// <reference types>` still resolves beside the referencing file.
`//tests/npm/type_roots` pins that.

### Finding a Broken Declaration

The baseline sets `skipLibCheck: true`, so a `.d.ts` whose own imports do not
resolve reports nothing. What it exports becomes `any`, and the first visible
error is elsewhere, typically a `TS7006` on a callback parameter in application
code.

`--//ts:lib_check` turns `skipLibCheck` off for every target in the build, over
whatever the target's tsconfig says:

```bash
bazel build //... --//ts:lib_check
```

It reports findings unrelated to the one being chased: a `lib` a dependency
needs and the program does not set reports here too. It is a diagnostic sweep,
not a build mode.

### What Is in Global Scope

A `.d.ts` with no top-level import or export is a **global script**, and
everything it declares belongs to every program the file is part of. Under the
chain a file joins the program by import, where a bare specifier resolves to
the package's module entry and no further, and by `types`: the chain's entries
or, when the chain sets neither `types` nor `typeRoots`, the direct `@types/*`
deps' names; a chain that sets `typeRoots` admits what those roots hold.
`@sentry/cloudflare`'s declarations import `@cloudflare/workers-types`, and
that resolves to the package's `index.ts`, a module; its `index.d.ts`, 15k
lines of global script, enters a program only through a `types` entry that
names the package. A worker names it in its tsconfig; a browser target that
depends on `@sentry/cloudflare` does not, and keeps `lib.dom`'s `Element`.

An import of a package the closure does not carry is `TS2307`. A `declare
module "x"` in a `.d.ts` src answers it, since nothing resolves the specifier
first.

### Importing Another Target by Bare Specifier

Two kinds of target answer a bare specifier. A pnpm workspace member is imported
through its importer's link target, `//<importer>:node_modules/<name>`, and
resolves through the store like any npm package; its `exports` map, rewritten
to the emitted files, decides what
`@acme/ui` and `@acme/ui/button` are. Any other first-party target is reached
through the tsconfig's `paths`: the rule rewrites each value to the source
directory and its `bazel-bin` twin, so a dep's declarations and a `ts_codegen`
`out_dir` tree resolve through the alias the tsconfig already has.

```jsonc
// apps/web/tsconfig.json
{ "compilerOptions": { "paths": { "#shared/*": ["../../packages/shared/src/*"] } } }
```

```python
# apps/web/BUILD.bazel
ts_compile(
    name = "web",
    srcs = ["main.ts"],          # import { flag } from "#shared/flags";
    tsconfig = "tsconfig.json",
    deps = ["//packages/shared"],
)
```

The value is read from the directory of the chain file that sets it, which
`--showConfig` does not print, so tsaction walks the `extends` chain for it. A
value import through an alias needs the module at runtime, which a dep on the
producing target gives a `ts_test` or `ts_binary`; a `ts_test` resolves the
alias to it as the compile did
([ts-test.md § A `paths` Alias](ts-test.md#a-paths-alias)).

### ts_config

Starlark cannot read a file, so a `ts_config` declares what a rule needs from
the tsconfig before any action runs: the `extends` chain, every file of which
becomes an input to the type-check action, and the two compiler options that
name an output -- `jsx = "preserve"` when that is the chain's effective `jsx`
([above](#a-tsx-under-jsx-preserve)), and `module` when the chain's is one
tsgo emits, which names the ES twins a vitest test runs
([The Module Format](#the-module-format)):

```python
load("@rules_typescript//ts:defs.bzl", "ts_compile", "ts_config")

ts_config(
    name = "tsconfig",
    src = "tsconfig.json",
    deps = ["//:tsconfig.base.json"],
    jsx = "preserve",
)

ts_compile(
    name = "lib",
    srcs = ["index.tsx"],
    tsconfig = ":tsconfig",
)
```

Gazelle writes all three from the chain
([the tsconfig and its ts_config](../gazelle/overview.md#the-tsconfig-and-its-ts_config)).
A tsconfig that extends nothing, does not set `preserve` and whose `module` is
an ES kind goes straight into `ts_compile`:

```python
ts_compile(
    name = "lib",
    srcs = ["index.ts"],
    tsconfig = "tsconfig.json",
)
```

### A Cloudflare Worker

Ambient globals from a generated `.d.ts`, no DOM, and no stray `@types` package
from the dependency graph reaching global scope, all in the worker's own
tsconfig:

```jsonc
// tsconfig.json
{
  "compilerOptions": {
    "lib": ["es2022"],
    "types": ["./worker-configuration.d.ts"],
    "resolveJsonModule": true
  }
}
```

```python
ts_compile(
    name = "worker",
    srcs = ["index.ts"],
    tsconfig = "tsconfig.json",
    deps = [":worker_types"],   # the ts_codegen whose outs write worker-configuration.d.ts
)
```

`types` names a package the chain resolves or a declaration file a dep stages
(both below). Neither puts a dep's globals in scope on its own; the entry does.

### A `types` Entry That Names a Package

`types: ["vite/client"]` names a package, and tsgo resolves it as tsc does over
an install: the package's own `types`, an `exports` subpath
(`@cloudflare/vitest-pool-workers/types`), a directory the package ships at that
subpath (`@cloudflare/workers-types/2023-07-01`), or the paired `@types/*`
package (`node` is `@types/node`). The package has to be on the chain -- an
importer's link, or an edge in a dep's closure -- and an entry the chain does
not answer is tsgo's `TS2688: Cannot find type definition file`, from the
action.

tsaction sets no `typeRoots` of its own. With a custom `typeRoots` tsgo skips
the `node_modules` walk for a `types` entry, and that walk is the only place an
`exports`-only subpath resolves; a chain that sets one keeps it, and its `types`
entries resolve under those roots alone, as they do in the checkout.

### A `types` Entry That Names a Declaration File

`types: ["./worker-configuration.d.ts"]` names a path. tsc resolves it against
the tsconfig's own directory, and tsaction lists the file as a root of the
program at the path the sandbox stages it, found with tsc's lookup: the path as
a file, the path with a TypeScript or declaration extension added, or a
directory's `index.d.ts`, in the source tree first and under `bazel-bin`
second. A checked-in declaration is in the source tree; a
generated one, such as the [`ts_codegen`](ts-codegen.md#cloudflare-worker-bindings)
running `wrangler types` writes, is in `bazel-bin`. What stages the file is a
label: `srcs`, or a dep whose `srcs` hold it or whose `outs` write it (a `.d.ts`
in `srcs` is passed through unchanged, so a dep edge stages it at the path the
entry names). Gazelle writes that dep from the tsconfig
([a declaration the tsconfig names](../gazelle/overview.md#a-declaration-the-tsconfig-names)).

An entry nothing stages fails the `TsConfig` action before tsgo runs, where
tsgo's own `TS2688` would name no dep:

```
tsaction: compilerOptions.types entry "./worker-configuration.d.ts" in
worker/tsconfig.json names worker/worker-configuration.d.ts, which no input of
this action sits at: not in the source tree, not under bazel-out/k8-fastbuild/bin.
A declaration this program names is a src of the target or an output of one of
its deps.
```

Only the two relative shapes are paths. `./x.d.ts` and `../x.d.ts` resolve
against the tsconfig's directory; anything else (`x.d.ts`, `vendor/x.d.ts`) is a
package name to tsc and to tsaction alike. A `../` entry reaches a file in an
ancestor directory, the way a test directory's tsconfig names the worker's
declaration beside the tsconfig it extends.

Such an entry is for globals. A module (a `.d.ts` with a top-level import or
export) resolves and joins the program, but its declarations stay scoped to it;
a module augmentation inside it needs the module in the program, which the entry
gives it.

### Ambient Precedence

The target's own declarations are root files, in the order the tsconfig's
`include` gives them and ahead of what `types` brings in -- a path-shaped entry
is listed after every src -- so where two declare the same `declare module`
pattern the one the tsconfig lists first wins, and the project's own beats a
`types` entry's. `tsc` orders them the same way: a
`types` entry arrives as a type-reference directive, which joins the program
after the root files. The first declaration of a pattern wins, and a narrower
pattern does not change that: an earlier `declare module "*.svg"` beats a
later `declare module "*.icon.svg"` even for `star.icon.svg`.

To let a package's ambient win instead, drop the project's competing
declaration.

### Which Ambients a Consumer Gets

A global `.d.ts` in `srcs` types the target that owns it and travels to every
consumer as a declaration output, and a consumer's program holds what its own
`include` and `types` name. So a consumer that needs the globals names the file
in its own tsconfig `types`, with the owning target in `deps`:

```python
# workers/proxy/BUILD.bazel -- worker-configuration.d.ts is a src of :proxy
ts_compile(
    name = "proxy",
    srcs = ["src/handler.ts", "worker-configuration.d.ts"],
    tsconfig = ":tsconfig",
    visibility = ["//visibility:public"],
)

# workers/proxy/test/tsconfig.json: "types": ["../worker-configuration.d.ts"]
# workers/proxy/test/BUILD.bazel
ts_test(
    name = "test_test",
    srcs = ["handler.test.ts"],
    tsconfig = ":tsconfig",
    deps = ["//workers/proxy", "@npm//:vitest"],
)
```

A package can hold an ambient it needs for its own standalone `tsc -p` that is
no part of its public type surface, such as a `process` shim in a library with no
`@types/node`, and no consumer sees it unless that consumer asks. The unit is
the file: TypeScript decides module-or-global per file. A `.d.ts` mixing a shim
for the package's own build with a declaration consumers are meant to have is
two files.

A consumer that uses a global no entry supplies sees the identifier as
undefined. Two things supply it: a `@types/*` dep of its own (`@types/node` for
`process`, in `types` or as a direct dep when the tsconfig sets neither `types`
nor `typeRoots`), or the owning target's file named in `types`.

## Which Tool Emits the Declarations

Every program is checked by `TsgoCheck`, a `--noEmit` run in the `_validation`
output group: it runs with `bazel build`, fails it on a type error, and no
dependent waits for it. The `.d.ts` a dependent's compile reads are
`TsInfo.declarations` and the `declarations` output group, never a default
output. The program's `module` decides which tool emits the JavaScript ([The
Module Format](#the-module-format)); `--//ts:declarations` decides which tool
emits the `.d.ts`, for every `ts_compile` in the build; a `ts_test`'s program
emits none ([The Test's Program](ts-test.md#the-tests-program)).

### `--//ts:declarations=tsgo` (default)

`TsgoDeclare` emits declarations from the complete type program -- the
target's sources, every first-party dep's `.d.ts` and the store files the
chain reaches -- with `--declaration --emitDeclarationOnly --noEmitOnError` on
its command line. It runs when a dependent's compile reads the target's
declarations, or on `bazel build //pkg:lib --output_groups=declarations`; a
leaf's `bazel build` runs the check alone.

- **No source annotations required.** Inferred export types are fine.
- **Declarations are exactly what `tsc` would emit**, including inferred object
  shapes, literal unions and `RegExp`.
- **A type error fails the build** in the check, and again in the declare,
  whose `noEmitOnError` leaves no `.d.ts` behind.
- **A dependent waits for the declaration emit**, the declaration transformer
  over the whole program, and never for the check; a `ts_test` under the
  target's tsconfig waits for neither, since it checks the sources ([The
  Test's Program](ts-test.md#the-tests-program)). The written tsconfig turns
  `declaration` off, so the check runs no transformer, and declaration
  diagnostics (`TS4xxx`) surface in the declare, where the declarations are
  emitted. A chain that sets `isolatedDeclarations` keeps `declaration` on,
  which the option requires, so its check runs the transformer and reports
  an unannotated export (`TS9xxx`) as `tsc -p` does.

### `--//ts:declarations=oxc`

Oxc emits declarations syntactically, per file, with no type program, beside
the `.js` in `TsEmit` -- tsgo's isolated-declarations emit does the same for a
CommonJS-shaped program. This requires
[isolated declarations](../getting-started/isolated-declarations.md): every
export needs an explicit type, and the emitter **errors** when one does not
have one. The check keeps `isolatedDeclarations`, with the `declaration` that
option requires, so tsgo reports the same rule and the declaration diagnostics
in the check. A dependent waits for Oxc's per-file transform alone.

Set it in `.bazelrc` once every package's exports are annotated.

## Cost of Each Mode

`tools/bench_declarations.sh 20 50 3` rebuilds 1,000 annotated files across
20 packages in one linear dependency chain under each mode, three interleaved
runs, and prints the wall and Bazel's critical path per run.

The benchmark's programs are ES modules, so Oxc emits the JavaScript and
`TsgoCheck` validates every package in both modes; the gap is the declaration
emit on the critical path. Under `tsgo` each of the 20 links waits for its
dependency's `TsgoDeclare`, the declaration transformer over the whole
program; under `oxc` it waits for Oxc's per-file transform. The gap shrinks on
shallower graphs and widens on deeper ones.

## Providers

The fields, and the load path, are in
[Providers and Toolchains](providers.md).

- **`TsInfo`**: this target's `.js`, `.js.map`, declarations and data srcs as
  direct depsets, the closure of the `.js` and data over its first-party deps
  as transitive ones, the closure's declarations per target in `owners`, plus
  the npm packages that closure imports. `ts_binary` reads the transitive `.js`
  set; `ts_test`, `ts_binary` and `ts_dev_server` stage `transitive_data`
  beside the `.js`; a downstream `ts_compile` type-checks against the `owners`
  records' declarations and stages `npm_files`
- **`OutputGroupInfo(declarations=...)`**: `TsInfo.declarations` as an output
  group, with the `.d.ts.map` under `--//ts:declaration_map`: what
  `bazel build --output_groups=declarations` and a
  `filegroup(output_group = "declarations")` request
- **`OutputGroupInfo(tsconfig=...)`**: the tsconfig this target handed the
  compiler, on any target with a program
- **`OutputGroupInfo(_validation=...)`**: the `TsgoCheck` stamp on every target
  with a program, and the `TsLint` stamp when the root module's `ts.lint()`
  names a linter ([Lint](../guides/lint.md)).

## Architecture

Three actions per target, each a function in `ts/private/actions/` --
`tsconfig.bzl`'s `TsConfig`, `emit.bzl`'s `TsEmit`, `tsgo.bzl`'s `TsgoCheck`
-- a fourth from `emit.bzl` under `--//ts:declarations=tsgo`, `TsgoDeclare`,
and a fifth, `lint.bzl`'s `TsLint`, when the root module's `ts.lint()` names a
linter ([Lint](../guides/lint.md)); the rule in
`ts/private/rules/ts_compile.bzl` declares the outputs, calls them in this
order and builds the providers.

`TsConfig` writes `<name>.tsconfig.json` and `<name>.options.json` from
`tsgo --showConfig` ([above](#where-compiler-options-come-from)). `TsEmit`
reads the options file's `module` ([The Module Format](#the-module-format)):
for an ES-module program it runs oxc over each root's `.ts` files with the
file's `target`, `jsx` and `jsxImportSource`:

1. Parse (oxc_parser)
2. Semantic analysis (oxc_semantic)
3. Isolated declarations emit (oxc_isolated_declarations), only under `--//ts:declarations=oxc`
4. TypeScript/JSX transform (oxc_transformer)
5. Code generation (oxc_codegen) for `.js` + `.js.map`

For a CommonJS-shaped program it runs `tsgo --noCheck` from the program root
below, with the emit shape on the command line, and moves the outputs into
place, each map's `sources` resolved to exec-root paths.

tsgo runs from the program root, with `--project` on the written tsconfig.
`TsgoCheck` adds `--noEmit` and `--explainFiles`; tsaction reads the listing
against the ownership manifest
([Deps Have to Be Direct](#deps-have-to-be-direct)) and writes a stamp for
`_validation`. `rootDir` in that tsconfig is the exec root, which every input
is under, since tsgo checks the program against it even when nothing is
emitted (`TS6059`). Under `--//ts:declarations=tsgo`, `TsgoDeclare` invokes
`tsaction emit -declarations_only` with the program inputs and source roots.
The emitter runs checked tsgo with `--declaration --emitDeclarationOnly
--noEmit false --noEmitOnError`, then projects its scratch outputs into the
[shared source layout](#shared-source-layout). It runs when declarations are
requested.

## Output Paths

Output paths derive from source file names within the
[shared source layout](#shared-source-layout), preserving relative imports such
as `import "./foo"` beside the emitted `foo.js`. Two `ts_compile` targets in
the same package therefore cannot list the same source file: Bazel reports
conflicting actions. Disjoint source lists also need a single publisher for any
shared runtime scope: independent `package_scopes = ["package.json"]`
declarations still write the same package.json output. Use the
[aggregate construction](#compiler-inputs-and-emitted-layout) above to publish
the complete projection once.

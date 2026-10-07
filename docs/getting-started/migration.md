# Migrating from rules_ts

`rules_typescript` is a fresh implementation, not a fork of `rules_ts` from
[aspect-build](https://github.com/aspect-build/rules_ts). `rules_ts` has a
release, production users and Windows support. This has none of the three.

## When to Use Which

**Choose `rules_ts` (Aspect) if:**
- You need full `tsc` compatibility for every TypeScript edge case, including decorator metadata
- You need Windows support today
- You want a BCR-published ruleset with production users
- You're already invested in `rules_js` and the Aspect ecosystem

**Choose `rules_typescript` (this) if:**
- You want Vite configured by the ruleset
- You want BUILD files from tsgo's program listing, one package per
  `tsconfig.json`, with occasional hand-narrowed attributes pinned by `# keep`

## Comparison

The Aspect column describes [rules_ts 3.10.1][aspect-module]. Its declared
minimum rules_js version is 2.0.0; the rules_js capabilities below are checked
at that version. BUILD generation uses the separate aspect-gazelle project.

| | rules_ts (Aspect) | rules_typescript (this) |
|---|---|---|
| **Compiler** | tsc or a configured [transpiler][aspect-transpiler] such as SWC; custom rules/macros supported | Oxc (Rust) for an ES-module program, tsgo (Go) for a CommonJS-shaped one |
| **Type-checker** | TypeScript tsc, including [native TypeScript 7][aspect-native] | tsgo |
| **Compilation boundary** | Per [`ts_project`][aspect-project]; `.d.ts` when declarations are enabled; each invokes `tsc --project` | `.d.ts` per target |
| **Bundler** | Bring your own | Bring your own, through `BundlerInfo` on `ts_binary` |
| **Dev server** | [`js_run_devserver`][aspect-devserver] (rules_js) runs the named binary or command; under ibazel it syncs changed `data` | oj 0.2.16 by default, optional Vite; any `DevServerInfo` rule per target |
| **npm management** | rules_js (pnpm virtual store, symlinks) | Own pnpm lockfile reader: a `pnpm-lock.yaml` is required, npm and yarn lockfiles are not read; one Bazel repository per package, fetched on demand; pnpm's virtual store as Bazel artifacts |
| **BUILD generation** | Can use [aspect-gazelle][aspect-gazelle] with its `js` language (Apache-2.0), from source or a prebuilt binary | Gazelle (one package per `tsconfig.json`) |
| **Framework support** | None built-in | None built-in; a framework's Vite plugin runs in the dev server through `vite_config` |
| **Bazel deps** | rules_js (rules_nodejs through it), aspect_bazel_lib, bazel_skylib, platforms, protobuf, rules_proto, aspect_tools_telemetry | rules_nodejs, rules_rs, llvm, rules_go + gazelle, rules_shell, bazel_skylib, platforms, toolchain_utils, package_metadata, rules_license |
| **Isolated declarations** | Not required | Not required; opt-in per package for throughput |
| **pnpm** | [Bazel-managed][aspect-pnpm]: `@pnpm//:pnpm`, fetched from npm; `npm_translate_lock(update_pnpm_lock = True)` runs it | Hermetic, always downloaded ([hermetic pnpm](../guides/npm.md#hermetic-pnpm)); Linux and macOS only |
| **BCR** | [Published][aspect-bcr] | Not published; no module or tools release, so consumers pin a commit |
| **Production users** | [Reported by the project][aspect-readme] | None yet |
| **Windows** | Supported | Not supported |

The dependency row lists declared modules, not tools every target executes;
`protobuf` and `rules_proto` are used by `ts_proto_library`.

When rules_ts separates transpilation from checking, a default `bazel build`
may not demand the check outputs. Its typecheck tests or the
[`validation_typecheck` option][aspect-typecheck] request them. This ruleset
puts checking in the build's validation outputs by default.

## Where rules_ts Is Better

### tsc Edge-Case Compatibility

Oxc is not tsc. Decorator metadata (`emitDecoratorMetadata`) may behave
differently, very new TypeScript syntax can lag tsc by a few weeks, and exotic
`tsconfig.json` options may not be handled identically. This applies to the
JavaScript transform only. Declarations come from tsgo by default, so the `.d.ts`
are what TypeScript itself would emit.

### Mature Ecosystem

`rules_ts` is published on the BCR and used in production. `rules_typescript`
has no tag, no BCR entry and no production users, and its API has broken
repeatedly pre-1.0. Read the [changelog](../changelog.md) when you move a pin.

### npm Handling

`rules_js`'s lockfile reader has seen more lockfiles than ours. Ours handles
scoped packages, `@types` pairing, multiple versions, peer sets (one store tree
per snapshot), npm aliases, pnpm workspaces, dependency cycles and pnpm's
hidden hoist; an exotic lockfile pattern may still break it.

### Windows

`rules_ts` + `rules_js` work on Windows. Windows is not supported here right
now; it may be considered in the future. See
[Compatibility](../compatibility.md#windows).

## Where rules_typescript Is Better

### Compilation Speed

Oxc is a Rust transformer with no type program, so the per-file transform is far
cheaper than tsc's. No like-for-like comparison against `rules_ts` has been run.
Measured against this ruleset itself: a rebuild of 1,000 files across 20 packages
after touching every source takes 6.3s with tsgo emitting declarations and 2.7s
with oxc emitting them and nothing type-checking. See
[Cost of each mode](../rules/ts-compile.md#cost-of-each-mode) for method and
caveats.

### Direct Dependencies

An import has to be satisfied by a direct dep. A declaration arriving through
another dep's own deps does not count, and the error names the label to add.
`rules_ts` passes the whole transitive closure to `tsc`, so a target can compile
against a dependency it never declared and break when an unrelated package drops
one. BUILD files must therefore be accurate; Gazelle writes them from the same
tsgo listing the check reads.

### Dev server choice

oj 0.2.16 is the default dev server and provides native React Fast Refresh.
Select Vite explicitly for framework Vite plugins. Both receive a generated
Vite-format config through `DevServerInfo`: `ts_dev_server(server = ...)` is a per-target
choice. With rules_js, [`js_run_devserver`][aspect-devserver] runs the binary
or command you name; under ibazel it syncs changed `data` files.

### No JS-Ruleset Layer

There is no `rules_js`: the ruleset reads `pnpm-lock.yaml` itself, declares
one Bazel repository per package behind a `@npm` alias hub, and builds pnpm's
virtual store from them as Bazel artifacts.
That is fewer moving parts in the JS layer and a larger dependency chain overall.
Oxc is Rust, so `rules_rs`, hermetic LLVM and a Rust toolchain come along, and the first
build pays for it. The ruleset's Go tools arrive as the binaries of its tools
release; a consumer compiles Gazelle, under `bazel run //:gazelle`, and the
compiler only when it registers
[tsgo from source](../rules/providers.md#tsgo-from-source); `rules_go`'s SDK
is fetched for those alone. `rules_ts` needs neither toolchain.

### Gazelle

One package per `tsconfig.json`, its deps from tsgo's own listing of the
program, without program-membership directives. `rules_ts` can use the `js` language of
[aspect-gazelle][aspect-gazelle], [Apache-2.0][aspect-gazelle-license], in a
Gazelle binary built from source or the `aspect_gazelle_prebuilt` binary.

## Migration Steps

If you decide to migrate from `rules_ts`:

1. Replace `ts_project` targets with `ts_compile`
2. Replace `js_library` / `npm_link_all_packages` with the `npm` module
   extension in `MODULE.bazel`:

   ```python
   npm = use_extension("@rules_typescript//npm:extensions.bzl", "npm")
   npm.translate_lock(pnpm_lock = "//:pnpm-lock.yaml")
   use_repo(npm, "npm", "pnpm")
   ```

   `"pnpm"` is the hermetic pnpm the `ts_pnpm` and `ts_add_package` targets
   you write beside the lockfile run, and the one that installs the checkout
   Gazelle lists. See [Setup](../guides/npm.md#setup).

3. Leave your `tsconfig.json` where it is, under its name. It is the package:
   step 4 writes `ts_config(name = "tsconfig", src = "tsconfig.json")` beside
   it, a `ts_compile` over what it lists with `tsconfig = ":tsconfig"`, and a
   `ts_test` over its test files, so the generated config extends yours; see
   [where compiler options come from](../rules/ts-compile.md#where-compiler-options-come-from).
   A repository of several projects has one `tsconfig.json` per project, each
   its own package. Deleting or renaming the file leaves no package: Gazelle
   reads only a file named `tsconfig.json`. Delete `baseUrl` from it;
   tsgo rejects the key
   ([Option 'baseUrl' has been removed](../guides/troubleshooting.md#option-baseurl-has-been-removed)).
   If you also run `ts_refresh_tsconfig`, which
   [overwrites the file at `tsconfig`](ide-setup.md#setup), point that at
   another name and extend it from yours
   ([Extending the generated file](ide-setup.md#extending-the-generated-file))
4. Run `bazel run //:gazelle` to regenerate BUILD files
5. Leave `compilerOptions.paths` alone. The rule reads it from your file and
   rewrites each value to its source and `bazel-bin` twins; tsgo resolves an
   aliased import through the same entries when Gazelle lists the program, and
   the package that owns the file it landed on goes into `deps`. No attribute
   repeats the alias; the
   [quickstart](quickstart.md#path-b-existing-project) shows the shape
6. Nothing else. Missing explicit return types are fine; the default emitter
   infers them

### Key Conceptual Differences

**The tsconfig is generated, and yours is what it extends.** `ts_compile` writes
a tsconfig per target that extends the ruleset's baseline and then your file,
referenced where it lives, and sets over both what Bazel owns: `rootDirs`, the
emit shape, `include` and `files`. It rewrites each `paths` value to its source
and `bazel-bin` twins and lists a path-shaped `types` entry as a root file.
Every other option is the file's; the rule
has no attribute for any of them.

**One Bazel repository per npm package.** Both fetch packages through
per-package repositories and build pnpm's virtual store:
[rules_js generates an `npm_import` per package][aspect-packages]. Here,
`@npm` holds aliases into the package repositories, and each lockfile's
package declares the store (`npm_virtual_store`). Consumer labels remain
`@npm//:react`, `@npm//:types_react`, `@npm//:vitest_bin`.

**Isolated declarations are a build flag.** `--//ts:declarations=tsgo`, the
default, needs no annotations; `--//ts:declarations=oxc` emits every `.d.ts`
from the annotated source alone, taking type-checking off the critical path.

**`node_modules` is the importer's.** Every lockfile importer's package holds
a `node_modules` target, its declared packages linked into the store, and
`ts_compile` and `ts_test` name the nearest one and resolve each npm dep along
it and its `parent`s, pnpm's walk-up; `ts_dev_server` and `ts_codegen` take
the same target. The layout is pnpm's: one store tree per resolution -- name,
version and peer set -- with its own edges beside it, and an importer's links
at the top.

[aspect-module]: https://github.com/aspect-build/rules_ts/blob/v3.10.1/MODULE.bazel
[aspect-transpiler]: https://github.com/aspect-build/rules_ts/blob/v3.10.1/docs/transpiler.md
[aspect-native]: https://github.com/aspect-build/rules_ts/blob/v3.10.1/ts/private/npm_repositories.bzl
[aspect-devserver]: https://github.com/aspect-build/rules_js/blob/v2.0.0/js/private/js_run_devserver.bzl
[aspect-pnpm]: https://github.com/aspect-build/rules_js/blob/v2.0.0/docs/pnpm.md
[aspect-readme]: https://github.com/aspect-build/rules_ts/blob/v3.10.1/README.md
[aspect-typecheck]: https://github.com/aspect-build/rules_ts/blob/v3.10.1/docs/troubleshooting.md#type-errors-are-not-reported-by-bazel-build
[aspect-packages]: https://github.com/aspect-build/rules_js/blob/v2.0.0/npm/private/npm_translate_lock.bzl
[aspect-gazelle]: https://github.com/aspect-build/aspect-gazelle/blob/19b94bd4d2671f7c9118b83cfeaa91850b08a054/language/js/README.md
[aspect-gazelle-license]: https://github.com/aspect-build/aspect-gazelle/blob/19b94bd4d2671f7c9118b83cfeaa91850b08a054/LICENSE
[aspect-project]: https://github.com/aspect-build/rules_ts/blob/v3.10.1/ts/private/ts_project.bzl
[aspect-bcr]: https://registry.bazel.build/modules/aspect_rules_ts/3.10.1

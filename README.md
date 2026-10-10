# rules_typescript

TypeScript programs are source-native by default: `ts_compile` and `ts_test` keep tsgo validation and emit no application JavaScript or declarations. Opt in with `emit = True` where a JavaScript-only runtime or declaration consumer requires built files; Gazelle follows those consumer requirements.

An opinionated Bazel ruleset for TypeScript, built around **Oxc and tsgo**. It builds TypeScript packages and provides a source-built dev server. For a different build model, see [aspect-build/rules_ts](https://github.com/aspect-build/rules_ts).

Rust and Go do the work: [Oxc](https://oxc.rs/) compiles an ES-module program and [tsgo](https://github.com/microsoft/typescript-go) a CommonJS-shaped one; tsgo type-checks. The default dev server is oj 0.2.22. [Vite](https://vite.dev/) remains an explicit option. [Gazelle](https://github.com/bazelbuild/bazel-gazelle) writes the BUILD files. Write `.ts`, run Gazelle, `bazel build //...`. The build reads no `node_modules/`. No system Node. Just Bazelisk.

Coming from an existing TypeScript repository: [Install](#install) is the short
path, and the
[Quick Start](https://mikn.github.io/rules_typescript/getting-started/quickstart/)
covers the migration questions.

**Full documentation: [mikn.github.io/rules_typescript](https://mikn.github.io/rules_typescript)**

## Key Ideas

- **Oxc compiles opted-in programs** — With `emit = True`, Rust-based TypeScript/JSX transformer: `.js` + `.js.map` per file, and `.d.ts` too under `--//ts:declarations=oxc`. A program whose `module` is CommonJS-shaped is tsgo's emit — see [The Module Format](https://mikn.github.io/rules_typescript/rules/ts-compile/#the-module-format).
- **tsgo type-checks** — Go port of TypeScript, and, with `emit = True`, emits declarations too: no export annotations required, and the `.d.ts` are what `tsc` would produce. The check is a validation on every target and fails `bazel build` on a type error; the declarations are emitted where a dependent reads them.
- **Vitest and OJ can consume TypeScript sources** — The default `emit = False` on compile and test targets retains tsgo validation while removing JavaScript and declaration emission from their runtime path. See [source-only programs](docs/rules/ts-compile.md#source-only-programs) for supported consumers and package manifests.
- **The dev server is swappable** — `ts_dev_server(server = ...)` takes any target providing `DevServerInfo`. oj 0.2.22 is the default; select `@rules_typescript//vite:dev_server` for Vite. What a server does not read is declared in its provider, so a target depending on a field its server ignores fails at analysis time naming both.
- **Isolated declarations** — annotate the exports and build under `--//ts:declarations=oxc`, and Oxc emits the `.d.ts` syntactically, so a dependent waits for a per-file transform rather than for tsgo's declaration emit, which shortens a deep dependency chain substantially. Opt-in, per build — see [Cost of each mode](https://mikn.github.io/rules_typescript/rules/ts-compile/#cost-of-each-mode).
- **Gazelle generates BUILD files** — one package per compiler program, rooted in a `tsconfig.json` or existing manifest entry points, with sources and deps read off tsgo's own listing, without program-membership directives. It regenerates the attributes it owns on every run and names every value it drops, so a value it cannot derive needs `# keep` — see [Attributes Gazelle owns](https://mikn.github.io/rules_typescript/gazelle/directives/#attributes-gazelle-owns).
- **Direct dependencies** — a source may import only what a direct dep provides. A declaration arriving through another dep's own deps does not satisfy an import: the build fails naming the file, the specifier and the label to add, and `bazel run //:gazelle` writes it.
- **How npm packages are fetched** — one Bazel repository per package, fetched on demand, behind a `@npm` alias hub, so a target fetches only its own dependency closure. pnpm's virtual store is built as Bazel artifacts — one cached tree per resolution (name, version and peer set) — and each importer's `node_modules` links into it, so a target resolves what its importer declared.
- **Only Bazelisk required** — Bazel fetches Node.js, the Rust and Go toolchains, and [pnpm](https://mikn.github.io/rules_typescript/guides/npm/#hermetic-pnpm). It builds the ruleset’s Go tools and Oxc from source. pnpm installs the checkout that Gazelle lists; build actions use Bazel’s npm store.

## Requirements

The only prerequisite is **Bazelisk** (or Bazel 9+). Bazel fetches the Rust and Go toolchains, Node.js, and the npm packages your targets reach. It compiles `oxc-bazel` and the four Go tools from source. Later builds can reuse their cached outputs. [Prebuilt Go tools](https://mikn.github.io/rules_typescript/RELEASE_PROCESS/#tools) are an explicit option.

Supported platforms: Linux x86_64, Linux ARM64, macOS x86_64, macOS ARM64.
**Windows is not supported right now. It may be considered in the future.** See
[COMPATIBILITY.md](COMPATIBILITY.md#windows).

**No module release has shipped.** There is no `v*` tag, no Bazel Central
Registry entry and no production users. No tools release has been published. Pre-1.0, any commit may break the API
with no deprecation window. Every released break is listed in
[CHANGELOG.md](CHANGELOG.md) with the edit it requires, and every unreleased one
in [changelog.d/](changelog.d/); read both before moving a pin. Full policy:
[COMPATIBILITY.md](COMPATIBILITY.md#versioning-policy).

Vite and vitest are your dependencies, not the ruleset's: they come from your
own lockfile, and the rules generate configuration for whichever version it
resolves to. The versions the tests exercise, and the places a generated config
is version-sensitive, are in
[COMPATIBILITY.md](COMPATIBILITY.md#vite-and-vitest).

## Temporary LLVM sandbox repair

LLVM BCR `0.8.21` can omit Clang builtin headers when a sandbox presents the resource directory as a symlink. Until an LLVM release includes the fix, consumers must copy [the resource-directory patch](oj/llvm-resource-directory.patch) into their workspace root and copy the explicit root `archive_override` from [the React example MODULE](examples/react-app/MODULE.bazel), which applies that patch to the checksum-pinned `0.8.21` archive. Bazel does not inherit dependency overrides from this ruleset. Remove the override when upgrading to the fixed upstream release; no release containing the repair is claimed yet.

The patch preserves the compiler's headers and shared resources while excluding its host runtime libraries. The measured RE builds pass without the patch because their inputs are materialised directories; that alone does not verify sandbox builds. The upstream submission is prepared for the maintainer; see [the affected LLVM rule](https://github.com/hermeticbuild/hermetic-llvm/blob/v0.8.21/toolchain/merged_resource_directory.bzl).

## Install

**Step 1.** Create `.bazelversion`:

```
9.2.0
```

**Step 2.** Add to `MODULE.bazel`. The ruleset is not on the Bazel Central
Registry yet, so `bazel_dep` alone has nothing to resolve against; pin it from
git:

```python
module(name = "my_project", version = "0.0.0")

bazel_dep(name = "rules_typescript", version = "0.2.0")
git_override(
    module_name = "rules_typescript",
    remote = "https://github.com/mikn/rules_typescript.git",
    commit = "REPLACE_WITH_A_COMMIT_SHA_FROM_MAIN",
)
register_toolchains("@rules_typescript//ts/toolchain:all")

bazel_dep(name = "gazelle", version = "0.47.0")
```

Pin a full commit SHA, not a branch. bzlmod still requires `version` on
`bazel_dep` and ignores its value while the override is active. The
`archive_override` (smaller fetch) and `local_path_override` forms are in
[Depending on rules_typescript](https://mikn.github.io/rules_typescript/getting-started/quickstart/#depending-on-rules_typescript).

**Step 3.** Add to `.bazelrc`:

```
build --incompatible_strict_action_env
build --nolegacy_external_runfiles
build --output_groups=+_validation
```

Those three lines are the whole file. Do not add an `@rules_rust` flag:
`rules_rust` is not exposed by `rules_typescript` to your
module, so Bazel cannot resolve the label and rejects the invocation with
`No repository visible as '@rules_rust' from main repository`.

**Step 4.** Add to `BUILD.bazel` at the repository root:

```python
load("@gazelle//:def.bzl", "gazelle")

gazelle(
    name = "gazelle",
    gazelle = "@rules_typescript//gazelle:gazelle_typescript",
    tags = ["manual"],
)
```

Point at `gazelle_typescript`, not `gazelle_ts`. `gazelle_ts` also carries the Go
and proto languages, for this repository's own `.go` sources, and in a polyglot
repo it rewrites Go BUILD files too.

**Step 5.** Write TypeScript, with a `tsconfig.json` in the directory that is
to be a package: Gazelle writes one `ts_compile` per `tsconfig.json`, over what
the program lists. Export annotations are optional: tsgo emits the declarations
from the full type program, so an inferred return type is fine:

```typescript
// src/lib/math.ts, beside src/lib/tsconfig.json
export function add(a: number, b: number) {
  return a + b;
}
```

**Step 6.** Generate BUILD files, build, and test:

```bash
bazel run //:gazelle
bazel build //...
bazel test //...
```

### Adding npm Dependencies

The npm extension is reproducible: consumers’ `MODULE.bazel.lock` contains no npm extension metadata. Bazel derives repository definitions from the watched pnpm lockfile, workspace/npmrc files, member manifests and patch bytes; changing those inputs refreshes the graph. Package downloads still use the lockfile’s integrity checks.

One-time setup in `MODULE.bazel`:

```python
npm = use_extension("@rules_typescript//npm:extensions.bzl", "npm")
npm.translate_lock(pnpm_lock = "//:pnpm-lock.yaml")
use_repo(npm, "npm", "pnpm")
```

`"pnpm"` is the hermetic pnpm behind the `ts_pnpm` and `ts_add_package` targets
you write into your root `BUILD.bazel`
([Hermetic pnpm](https://mikn.github.io/rules_typescript/guides/npm/#hermetic-pnpm)).

Then, per package:

```bash
bazel run //:pnpm -- add zod   # updates pnpm-lock.yaml and installs it
bazel run //:gazelle           # lists each tsconfig.json with tsgo, writes deps
bazel build //...              # fetches just that package's closure, builds
```

Bazel fetches a package the first time a target needs it. Build actions use Bazel’s npm store. Gazelle and editor tools can read the checkout’s installed `node_modules/`; keep those directories out of Git.

`bazel run //:pnpm -- add zod --lockfile-only` uses a hermetic pnpm
([two lines of setup](https://mikn.github.io/rules_typescript/guides/npm/#hermetic-pnpm)).

## IDE Integration

`ts_refresh_tsconfig` writes the workspace-root `tsconfig.json` from Bazel's
build graph: one `compilerOptions.paths` entry per first-party package your
targets reach, source directory and `bazel-bin` twin. The file is checked in,
and `test = True` adds a test that fails once it goes stale. An editor, a plain
`tsc` run and a coding agent's language server resolve Bazel's declarations
through it with no setup; npm packages resolve through the checkout's
`node_modules`, so `pnpm install` is the editor's npm setup. A tsserver plugin
installed alongside it resolves live, without a re-run; the plugin needs editor
configuration.

```python
# BUILD.bazel
load("@rules_typescript//ts:defs.bzl", "ts_refresh_tsconfig")

ts_refresh_tsconfig(
    name = "refresh_tsconfig",
    test = True,
    deps = [
        "//apps/web",
        "//packages/design-system",
    ],
)
```

`deps` is the whole input. An aspect walks it, so listing a target covers
everything it depends on; the default, `deps = []`, writes an empty `paths`. It
obeys visibility, so a package-private target cannot be listed.

```bash
bazel run //:refresh_tsconfig        # writes tsconfig.json and the plugin
bazel test //:refresh_tsconfig_test  # fails when the checked-in tsconfig is stale
```

The plugin is optional. To turn it on, point tsserver's plugin probe at `.bazel`
and name `@rules_typescript/tsserver-plugin`. The per-editor recipes, and the
coding-agent case, are in
[IDE Setup](https://mikn.github.io/rules_typescript/getting-started/ide-setup/#editor-configuration).

`nested_tsconfigs` lists the packages that need their own editor program, as
workspace-relative paths to the `tsconfig.json` each one gets. A package belongs
there when its targets set `compilerOptions` the root block cannot also be set
to. The list is declared, not discovered. The rule fails at analysis time when
the list disagrees with the graph in either direction, so a repository with one
such package fails the snippet above until the list is filled in. That attribute,
`extra_exclude` and the other editors are in
[IDE Setup](https://mikn.github.io/rules_typescript/getting-started/ide-setup/).

## Documentation

- **[Quick Start](https://mikn.github.io/rules_typescript/getting-started/quickstart/)** — new project or migrating an existing codebase
- **[IDE Setup](https://mikn.github.io/rules_typescript/getting-started/ide-setup/)** — a generated `tsconfig.json` plus live tsserver resolution from Bazel's build graph (TypeScript's GOPACKAGESDRIVER)
- **[Isolated Declarations](https://mikn.github.io/rules_typescript/getting-started/isolated-declarations/)** — the opt-in throughput mode
- **[npm Dependencies](https://mikn.github.io/rules_typescript/guides/npm/)** — pnpm lockfile integration, platform-specific packages, bin scripts
- **[Testing with vitest](https://mikn.github.io/rules_typescript/guides/testing/)** — `ts_test`, snapshots, sharding, watch mode with ibazel; `runner = "@rules_typescript//ts/runners:node_test"` for tests written against node's own runner
- **[Bundling](https://mikn.github.io/rules_typescript/guides/bundling/)** — `ts_binary` with any `BundlerInfo`-compatible bundler
- **[Dev Server](https://mikn.github.io/rules_typescript/guides/dev-server/)** — a pluggable dev server with ibazel HMR: oj by default, optional Vite or another `DevServerInfo` rule through `server`
- **[Monorepo Layout](https://mikn.github.io/rules_typescript/guides/monorepo/)** — one package per `tsconfig.json`, cross-package `.d.ts` caching
- **[Gazelle Reference](https://mikn.github.io/rules_typescript/gazelle/overview/)** — what a run reads and writes, `# keep`
- **[Protobuf sources](docs/rules/ts-proto-library.md)** — `ts_proto_library` over `proto_library`, protoc, pinned protobuf-es and source-mode TypeScript
- **[Rules Reference](https://mikn.github.io/rules_typescript/rules/ts-compile/)** — all attributes, providers, and outputs
- **[Migration from rules_ts](https://mikn.github.io/rules_typescript/getting-started/migration/)** — differences from aspect-build/rules_ts
- **[Troubleshooting](https://mikn.github.io/rules_typescript/guides/troubleshooting/)** — the error messages, by message text
- **[Benchmark](https://mikn.github.io/rules_typescript/guides/benchmark/)** — Matching work, cache states and invocation evidence
- **[Compatibility](https://mikn.github.io/rules_typescript/compatibility/)** — Bazel and platform support, the Vite/vitest versions the tests exercise, and what "pre-1.0" means here

## License

MIT

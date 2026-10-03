# AGENTS.md for rules_typescript

Instructions for AI agents and contributors working on this codebase.

**This file is a living document.** When you discover important patterns, preferences, or lessons about working on this project, add them here. Keep it terse.

## Quality Standard

This ruleset targets **rules_go ergonomic parity**. The bar: a TypeScript developer writes `.ts` files, runs `bazel run //:gazelle`, then `bazel build //...` and `bazel test //...`, with no manual BUILD file editing.

## Contribution Workflow

**Always use the PR workflow. Never push directly to main.**

```bash
# 1. Create a branch
git checkout -b feat/my-feature

# 2. File an issue first (for non-trivial changes)
gh issue create --title "..." --body "..."

# 3. Develop using the three-stage cycle (see below)

# 4. Create a PR
gh pr create --title "feat: ..." --body "Fixes #N"

# 5. After review + CI green, merge
gh pr merge --squash
```

**Issue tracking:**
```bash
gh issue create --title "..." --body "..." --label "enhancement"
gh issue create --title "..." --body "..." --label "bug"
gh issue list
```

## Development Workflow

```bash
bazel test //...                           # everything; `bazel query 'tests(//...)' | wc -l` for the count
bazel test --config=fast //...             # skips the nested-Bazel integration tests
bazel build //... --output_groups=+_validation  # redundant if .bazelrc has it
cd e2e/basic && bazel test //...           # e2e workspace (in .bazelignore)
cd examples/react-app && bazel test //...  # example workspace (in .bazelignore)

# One integration test on its own. Each spawns a nested Bazel and is tagged
# `nested-bazel` and `cpu:2`: --config=fast drops them, and a full run bounds
# how many run at once by the machine's cores.
bazel test //tests/integration:new_project_test
```

## Three-Stage Development Cycle

For any non-trivial change:

1. **Implement** — write code, build, test, iterate until green
2. **Adversarial review** — separate agent finds bugs, design flaws, shell injection, depset violations
3. **Fix** — address all CRITICAL and HIGH findings, verify

Do not skip the review stage.

## Architecture

```
ts_compile → TsConfig action (<name>.tsconfig.json + <name>.options.json from
             `tsgo --showConfig` over the baseline and the target's tsconfig)
           → TsEmit action (.js + .js.map; + .d.ts under
             --//ts:declarations=oxc): oxc for an ES-module program,
             `tsgo --noCheck` for a CommonJS-shaped one, by the options
             file's module
           → TsgoCheck validation action (.tscheck stamp in _validation) on
             every program: --noEmit --explainFiles, failing an edge from a
             src into a file a label outside deps owns (the <name>.ownership
             manifest names the owner)
           → TsgoDeclare action (.d.ts, the `declarations` output group and
             TsInfo.declarations; under --//ts:declarations=tsgo) when a
             dependent's compile reads them or the group is requested;
             tsaction emit -declarations_only checks the full program and
             projects scratch declarations through the shared source layout
           the tsgo runs link declared Files at logical source paths in a
           program root, with generated roots in their original package
           scopes and bazel-out linked whole, each importer's
           node_modules at the importer's directory, the chain
           `node_modules` names; each run's emit shape is its command line
           → TsLint validation action (.tslint stamp in _validation) when the
             root module's ts.lint() names a linter

.ts = the default runtime boundary. With `emit = False`, a program
retains TypeScript sources and validation without JavaScript or declaration
emission; source-capable runners transform those inputs. Downstream emitted
programs otherwise see .d.ts, not .ts source;
a ts_test under the target's tsconfig is the exception, one program with the
compile over the .ts (docs/rules/ts-test.md § The Test's Program).
Change implementation without changing .d.ts → no downstream recompilation.

The strict-deps check is the tsgo action's: tests/strict_deps pins the manifest
it reads, and //tests/integration:new_project_test the failing build.

The rule takes srcs, data, deps, tsconfig, node_modules and emit. Every compiler
option is the tsconfig's, read by tsaction; the emit knobs are the flags in
ts/BUILD.bazel
(//ts:declarations, //ts:source_map, //ts:declaration_map, //ts:lib_check);
//ts:checkers sizes tsgo's threads and the cpus its two actions declare.
```

**Key files:**
- `ts/defs.bzl` — TypeScript compile/runtime API (rules, providers, macros)
- `proto/defs.bzl` — `ts_proto_library`: ProtoInfo sources through protobuf protoc and pinned protobuf-es, then the existing source-mode compiler; `docs/rules/ts-proto-library.md` names graph and option ownership
- `ts/private/rules/ts_compile.bzl` — the `ts_compile` rule and
  `TS_COMPILE_ATTRS`; `ts/private/actions/` — one action per file (`tsconfig`,
  `emit`, `tsgo`, `lint`, `vitest`, `workers_pool`), the functions the rules
  call in that order; `lint.bzl` also holds `lint_config` and the repository rule
  `ts.lint()` writes
- `ts/tools/tsaction/` — the Go runner behind the actions: `tsconfig` writes the action config from `tsgo --showConfig`, `oxc` relays the options to oxc, `tsgo` lays out the program root, runs tsgo from it and checks the listing's edges against the ownership manifest
- `ts/tools/explainfiles/`, `ts/tools/tsconfig/`, `ts/tools/jsonc/` — the
  `--explainFiles` grammar, the tsconfig `extends` chain reader and the JSONC
  parser, shared by tsaction and Gazelle
- `ts/private/node_modules.bzl` — `node_modules` and `node_modules_member`, an importer's links into the store: the chain `ts_compile` and `ts_test` resolve a direct npm dep along
- `ts/private/providers.bzl` — TsInfo, TsTestRunnerInfo, TsConfigInfo, NpmPackageInfo, DevServerInfo, BundlerInfo, NodeModulesInfo, NpmHoistInfo, NpmLinkInfo
- `npm/private/npm_translate_lock.bzl` — pnpm lockfile reader, and the registry
  map from `.npmrc` and `pnpm-workspace.yaml` (parsing only; no repository rule)
- `npm/extensions.bzl` — the `npm` module extension (translate_lock, pnpm tags)
- `npm/lazy.bzl` — whole-graph analysis + one `npm_import` per package + the alias hub
- `npm/private/npm_import.bzl` — the per-package repository rule and `npm_hub`
- `npm/private/workspace_package.bzl` — the hub's view of a workspace member
- `npm/private/fetch_auth.bzl` — the credentials a fetch sends: the `.npmrc`'s
  lines and pnpm's `auth` setting, `PNPM_CONFIG__AUTH`; loaded by `npm_import`
  and the tsgo toolchain's repository rule
- `ts/private/pnpm.bzl` — hermetic pnpm download + `ts_pnpm`/`ts_add_package` macros
- `ts/private/tsgo_lock.bzl` — which compiler a pnpm lockfile pins, the reader behind `ts.tsgo(pnpm_lock = ...)`; `ts/private/tsgo/pnpm-lock.yaml` is the default
- `ts/private/go_mod.bzl` — the go.mod and go.sum readers behind the source-built tsgo; `ts/private/tsgo_source/` holds the compiler's pin (go.mod) and the patch the build applies, `//ts/toolchain/tsgo_source` is the toolchain over rules_go's build of it, outside `//ts/toolchain:all`; `//ts/private/tsgo:tsgo_test` holds the pin to the lockfile's binary
- `ts/private/ts_config.bzl` — the public `ts_config` rule (a hand-written tsconfig.json and its `extends` chain)
- `platforms/platforms.bzl` — the one platform table (`PLATFORMS`) everything loads
- `ts/toolchain/BUILD.bazel` — the six toolchain types and their instances;
  `//ts/toolchain:all`; `ts/private/tools_lock.bzl` names the tools release the
  tools and launcher instances download, and `//ts/tools/tsaction:
  source_toolchain` and `//tools/launcher:all` are the source-built instances
  this workspace registers first
- `ts/private/rules/ts_test.bzl` — the `ts_test` rule over `TS_COMPILE_ATTRS`
  and the test attributes; `ts/private/rules/runners.bzl` — the two runner
  targets under `//ts/runners`, `vitest` and `node_test`, each providing
  `TsTestRunnerInfo`; `ts/private/actions/vitest.bzl` — the generated vitest
  config; `ts/private/actions/workers_pool.bzl` — the Workers pool's half of
  the test environment, the one file that names wrangler
- `ts/private/bundle_action.bzl` — the bundle action behind `ts_binary`'s `bundler` attr
- `ts/private/ts_dev_server.bzl` — dev server with HMR
- `ts/private/ts_codegen.bzl` — general code generation
- `ts/private/tsconfig_aspect.bzl` — the IDE tsconfig, the hook data, and the
  aspect that writes per-target fragments
- `tools/launcher/` — the one Go launcher `ts_binary`, `ts_test`,
  `ts_dev_server` and `npm_bin` run through; `--dump-config` prints the
  resolved per-target JSON config
- `gazelle/program.go`, `gazelle/owner.go` — the tsgo listing per
  `tsconfig.json`; the packages and `owner(f)`
- `gazelle/npm.go`, `gazelle/manifest.go` — the lockfile gate, the
  importer-scoped label, the member view; the nearest `package.json`
- `gazelle/generate.go`, `gazelle/resolve.go` — the package's rules; `deps`
  from the listing's edges
- `gazelle/workers_pool.go` — the Workers pool's half: the wrangler config's
  `filegroup`, the pooled test's `wrangler_config` and `coverage_provider`
- `gazelle/config.go`, `gazelle/keep.go` — the root-once lockfile load, the
  foreign-project mark, the `ts_codegen` bookkeeping; the managed-attribute
  reports
- `oxc_cli/src/main.rs` — Rust CLI (parse → isolated_declarations → transform → codegen)

## Rules

**Starlark:**
- Never materialize depsets at analysis time (no `.to_list()` in rule impls unless unavoidable + commented)
- `depset(order = "postorder")` for all transitive file sets
- `ctx.actions.run` only. `ctx.actions.run_shell` is gone from the ruleset and
  nothing new may reintroduce it
- Shell strings: always use `_shell_escape()` for any interpolated path
- All `fail()` calls must have actionable messages with "Did you mean...?" suggestions
- No Python dependencies. Use awk or Starlark `json.decode()`.

**Bazel:**
- bzlmod only. No WORKSPACE.
- Never reference `bazel-out/` directly. Use `ctx.bin_dir.path`, `File.path`.
- Optional toolchains: `config_common.toolchain_type(TYPE, mandatory = False)`
- Validation actions in `OutputGroupInfo(_validation = ...)`, not separate targets
- No `bazel clean`. Iterate. Trust the cache.
- Consumer toolchain registration is explicit: `register_toolchains("@rules_typescript//ts/toolchain:all")`

**Gazelle (Go):**
- Ordinary TypeScript membership needs no directives: a package is a directory
  whose `tsconfig.json` lists a first-party file, its program is that listing,
  and its deps come from the listing's edges, the lockfile and the nearest
  `package.json`
- A `ts_test` runs in the importer chain its `node_modules` names: its npm
  deps resolve to the chain's links, and each `ts_compile` dep's store files
  come along (`TsInfo.npm_files`)
- Register new rules in `Kinds()` + `Loads()`
- `bazel run //gazelle -- -mode=diff` on a clean tree must print nothing. A
  fixture that differs only in Gazelle's own rendering (a one-element list
  inline, a genrule's output filename over its label) makes real drift
  indistinguishable from formatting. Fix the fixture, or pin the hand-written
  form with `# keep`; `visibility` merges, so without `# keep` a hand-narrowed
  one comes back `//visibility:public` every run

**Testing:**
- Every feature needs a test that ASSERTS correctness (not just "builds without errors")
- Integration tests (`tests/integration/`) test full user journeys in a nested Bazel workspace: create project, gazelle, build, test
- They are part of `bazel test //...`. Tagging a test `manual` takes it out of CI; use `exclusive` when the problem is concurrency, not the test.
- `tests/bootstrap` is deleted. It was a non-hermetic duplicate of `tests/integration` (inherited PATH/HOME/USER, host `bazel`). Do not recreate it.
- Every integration workspace shares one repository cache. Each nested Bazel has
  its own output base; without the shared cache each fetches the whole BCR
  registry and the concurrent lookups fail on a different subset each run, which
  reads as a flaky test. The harness appends
  `common --repository_cache=` to each staged workspace's `.bazelrc`
  (`shareRepositoryCache` in `tests/integration/harness/harness.go`); do not add a
  workspace that bypasses `prepare()`.
- Use `sh_test` for output verification, `go_test` for Gazelle logic, vitest for runtime behavior
- `tools/ci/check_retired_names.sh`: a retired attribute, kind, provider,
  directive, export or path is named in `changelog.d/` and nowhere else. A file
  asserting the absence goes in its `ALLOWED` list with why
- `tools/ci/check_coverage_report.sh`: `bazel coverage` on the fixture, its
  report's `SF:` lines against what `--instrumentation_filter` selects; the
  suite never runs coverage and an empty report passes

**npm:**
- pnpm is hermetic (`bazel run //:pnpm`). No system pnpm needed.
- `--lockfile-only` adds a package to a fixture lockfile. The build reads no
  `node_modules/` from the source tree; Gazelle's listing does, so a workspace
  whose root lockfile it reads is installed first. This repository has no root
  lockfile: its lockfiles are fixtures under `tests/`.
- npm aliases (e.g., `h3-v2: npm:h3@2.0.1-rc.16`) must produce both the alias and real targets
- Dependency cycles broken by `break_cycles` in `npm/lazy.bzl`: a depth-first walk that drops each edge closing a cycle

## Provider Contract

Every `ts_compile` target provides: `TsInfo` + `InstrumentedFilesInfo` +
`OutputGroupInfo(_validation, declarations)`; a `ts_test` runs the same
actions over its srcs -- the emit as ES modules under the vitest runner -- and
provides the last two. `_validation` holds the `TsgoCheck` stamp and the
`TsLint` stamp when the root module's `ts.lint()` names a linter;
`declarations` holds the `.d.ts`, which are no default output.
Every `ts_npm_package` provides: `TsInfo`, naming its closure in
`npm_packages` and nothing by path, + `NpmPackageInfo` (whose `store` is the
snapshot's tree and the links beside it); a consumer reaches the package
through the chain its `node_modules` names, and its `TsInfo.npm_files` carries
the store files its action stages.
A data src of a `ts_compile` -- a `.css`, an image, a `.json` -- travels in
`TsInfo.transitive_data`, which `ts_test`, `ts_binary` and
`ts_dev_server` stage beside the `.js`; a `*.module.css` is Vite's own CSS
modules wherever Vite runs it.
`ts_compile.data` stages explicit standalone assets at package-relative paths
through the same provider. Emitted modules, imported JSON and scopes use one
logical source layout in the target's output namespace. A source-mode consumer
of a relocated dependency stages unchanged TypeScript in that layout while
compiler inputs retain their original identities; all-source closures keep
original runtime paths. Owner `runtime_files` and optional `declaration_files` record
exact source/output File pairs. Consumers link canonical dependency outputs
and overlay declarations at their source-origin paths, including when a
single-package consumer imports a relocated producer. Runnable consumers
validate exact runtime File and package-scope identity. Optional owner
`canonical_links` records exact `(link File, canonical File)` pairs for dependency
links and scope symlinks the producer creates without rewriting. npm member stores
may copy metadata-only scope aliases; they reject module and declaration aliases
because copying them loses canonical scope and importer identity.
`type_inputs` retains compiler-only metadata without runtime or source-root ownership.
Move foreign standalone JSON assets to `data` for package-local placement.
`ts_test.data` remains extra runfiles.

## npm Internals

One repository per package is the only implementation. The `npm_translate_lock`
repository rule and `npm.translate_lock(lazy = ...)` are deleted; do not
reintroduce a second resolver.

`npm/extensions.bzl` → `npm/lazy.bzl` (whole-graph analysis, no network) →
one `npm_import` per package + one `npm_hub` of aliases.

The analysis stays in the extension because none of it is a decision a package
can make about itself: platform filtering, which package a bare label means
(highest version), `@types` pairing, cycle breaking (`break_cycles`), alias
naming, patch routing. Each package then reads its own `package.json` and writes
its own BUILD file, which is what makes on-demand fetching possible. The tarball
is extracted under `node_modules/<name>/` inside the repository, the segment
TypeScript reads to classify a file under it as a library file; an importer's
`node_modules` links each package at `node_modules/<name>` into its store tree
from `NpmPackageInfo.package_name` and `store`, so nothing else spells the
layout.

Handled: scoped packages, `@types` pairing, multiple versions with
version-suffixed labels, bin scripts (fixed `:bin` alias per package, since the
hub cannot know whether a bin exists without downloading), conditional exports,
pnpm workspaces and `workspace:*` links, npm aliases (their own labels),
dependency cycles, `patchedDependencies` (verified four ways at extension time:
a label resolving to no readable file, a file whose sha256 disagrees with the
lockfile digest, a declared patch with no label, and a file no entry claims).

No code needed, pinned by tests: catalogs, overrides (including `parent>child`),
packageExtensions. pnpm resolves all three at every use site.

`node_modules` trees are flat where flat is unambiguous and keyed by resolution
where it is not: a name's primary resolution keeps the top-level directory, every
other one gets its bytes once under
`.pnpm/<name>@<version>[_<peer set>]/node_modules/<name>`, and each dependent
that resolved to one of those gets a relative symlink. The manifest the builder
reads is `op \t source \t destination`: `C` copy, `L` directory symlink and `S`
file symlink, copies first so no link is ever dangling.

A resolution is name, version and peer set: pnpm resolves a package once per
distinct peer set and the outcomes have different dependency edges, so
`NpmPackageInfo.peer_id` carries pnpm's peer suffix (the same token the
snapshot's repository name is built from) and everything keys on it. Two
store Files for one name at the same importer link location are an error;
different importer locations can retain different versions or peer sets in
one target.

A package's `exports`, `types`, `typings`, `main` and the `/// <reference
types>` headers of its declarations are read by nothing here: tsgo and node read
the manifest where the importer's link leads, as they do over an install, and
`NpmPackageInfo` carries no entry point. The one manifest the rules write is
the `package.json` at a `ts_compile`'s package root as built, `<name>.package.json`
beside the src staged as written: `tsaction manifest`
(`ts/tools/tsaction/manifest.go`) rewrites every source-file target under
`main`, `module`, `browser`, `exports` and `imports` to the emitted `.js` (the
`.jsx` for a `.tsx` under the target's declared `jsx: preserve`) and every
`types` target to the `.d.ts`, key order kept (an `exports` condition map is
read in the order it is written); the member's store tree copies it as its
`package.json`, and a dependent's program root lays it at the member's path. A
test's runfiles hold the src as written. `manifest_test.go` is the table.

## Dev Server Generated Config

Three invariants in `ts/private/ts_dev_server.bzl`.

**npm resolution is a `node_modules` link, with a plugin behind it.** Vite has no
search-path option: `resolve.modules` is webpack's, and Vite ignores it. Vite
resolves a bare specifier by walking up from the importer, or from `root` for
`resolve.dedupe` and `optimizeDeps.include`; neither walk goes through the
plugin container, so the launcher links the npm tree in as
`<workspace>/node_modules` (`linkAs` in `tools/launcher/plan.go`).
`bazel:npm-resolve` is `enforce: 'post'`, for an importer the walk cannot reach:
it locates `<tree>/<pkg>/package.json` and, if that exists, hands the id back to
`this.resolve()` with that manifest as the importer. At `'pre'` it rewrites every
bare importer into the tree, which Vite reads as a node_modules-internal import
and opts out of dependency optimisation. Exports maps, conditions and subpaths
stay Vite's to interpret; do not reimplement them here. A package the tree does
not carry returns `null`, so the user sees Vite's own unresolved-import error.

**An entry point comes from the package's `exports` map, never from a path into
its `dist/`.** `@vitejs/plugin-react` moved its entry between the two majors this
repo has built against, so a fixed `dist/index.mjs` is wrong for one of them.
`npmEntryPath` reads the manifest; a load failure throws, naming the label and
the dep to add.

**`vite_config` is loaded from a copy in bin; that is the hermeticity boundary.**
Node resolves a runfiles symlink before it resolves that file's own imports, so a
config loaded from the source tree resolves through a source-tree `node_modules`
this ruleset does not have. A bare npm specifier in the copy resolves through the
`node_modules` tree, which therefore has to be in the same Bazel package. A
relative import does not resolve, because only the one file is copied; the
server dies naming it. `//tests/dev_server:vite_config_boundary_test` pins all
three sides.

## Vite and vitest Versions

Neither is a ruleset dependency; both come from a consumer lockfile, and the rules
generate config for whatever it resolves to. `MODULE.bazel` translates six
lockfiles into six hubs. Four resolve Vite (`@npm`, `@npm_tailwind`,
`@npm_workers`, `@npm_eslint`), all at 8.2.2, with vitest 4.1.11 wherever a hub
resolves vitest at all, so there is one lane. `@npm` (`tests/npm/pnpm-lock.yaml`)
carries most of it: `tests/vitest/**`, `tests/dev_server/**` and `vite/tests/**`
(vite-plugin-bazel's own tests). The integration workspaces that import npm
(`tests/integration/gazelle_roundtrip`, `npm_deps`, `lsp`) carry their own
lockfiles, translated by the nested Bazel: the first two resolve the same 8.2.2
and 4.1.11, `lsp`'s resolves neither tool. `@npm_features` (`tests/npm/features/pnpm-lock.yaml`, declared
`dev_dependency`) is the pnpm patch/alias/peer-variant fixture and resolves
neither tool; `@npm_esbuild` (`vite/esbuild/pnpm-lock.yaml`) holds the esbuild
that bundles `vite-plugin-bazel` and resolves neither either. The per-hub table is in
COMPATIBILITY.md § Vite and vitest:

```bash
grep -rnE '^  (vite|vitest)@' --include=pnpm-lock.yaml .
```

A second hub on a second major is not a second lane, only a second lockfile to
keep in step: `@npm_vite` had the bundler on Vite 8 while the integration test
of the same rule ran on Vite 6. The coupling is a test:
`//vite/tests:peer_version_test` reads `peerDependencies.vite` out of
`vite/package.json` and asserts the installed major is one that range names, so
widening the range is an edit to both files.

One version-sensitive spot in the generated config: `ts_test`'s array-`config`
form emits `test.projects` (vitest 4 throws on `test.workspace`).

## Snapshots Under Bazel

`ts_test` redirects `test.resolveSnapshotPath` to
`<package>/__snapshots__/<source>.snap`, where a plain `vitest` keeps it, reads
those files from the runfiles as srcs of the test, and runs vitest in read-only
snapshot mode (`CI=true`), so no `bazel test` can write a `.snap` and pass on
what it wrote. Writing one is vitest's own `vitest -u` in the package.

## Anti-Patterns

- Don't add Python dependencies. All codegen uses awk or Starlark `json.decode()`.
- Don't generate bash scripts for Windows compatibility paths. Use Node.js via the runtime toolchain, or the Go launcher for anything runnable. Runners and the store copier are Go; no build action runs a shell.
- Don't create separate `_check` targets. Use `_validation` output group on the compile target.
- Don't assume `@npm` is the only repo name. Support custom names via the npm extension.
- Don't push directly to main. Use PRs.
- Don't skip the integration tests when adding new features, and don't tag them `manual` to make a run faster.

## Lessons Learned

- **End-to-end tests catch real bugs.** The nested-Bazel journey tests found 5 bugs on their first run, including a Rust binary bug where oxc-bazel ignored the isolated-declarations flag. They also spent a release tagged `manual`, which is how "34 pass" came to read as full coverage of a 54-target suite.
- **Shell escaping is never optional.** Every path interpolated into a shell string must use `_shell_escape()`. Three separate review rounds caught injection vectors.
- **npm alias support is non-obvious.** pnpm's `"h3-v2": "npm:h3@2.0.1-rc.16"` pattern requires both the alias name AND real name as `ts_npm_package` targets with different `package_name` values.
- **`bazel clean` is never the answer.** If the build is broken, the bug is in the rules, not the cache. Fix the root cause.
- **Every `fail()` should tell the user what to do.** "Did you mean...?" suggestions prevent hours of debugging.
- **Two recognisers of one thing drift.** Gazelle's own import lexer and the strict-deps checker had to agree specifier for specifier, or a hard error became unfixable by the tool meant to fix it; Gazelle reads tsgo's listing now, the resolution the build checks. Same shape as the `node_modules` tree: the layout planner and the builder read one manifest, not two ideas of it.
- **A name is not a resolution.** Keying anything by npm package name alone (a `node_modules` destination, a patch pairing, a dep edge) loses the version and fails silently, because every version involved is a real version. `name@version` is one key short too: pnpm resolves once per peer set.
- **A green suite is not a preserved suite.** `bazel run //gazelle` once deleted hand-written `go_test` targets and still satisfied "builds" and "idempotent"; a deleted test passes both. `bazel query 'tests(//...)'` before and after is the check that catches it, and it is now part of the Gazelle acceptance run.
- **A test that never ran is not a test.** `tests/vitest/environment` was two `manual` targets behind a `build_test`, so no non-default vitest environment had ever executed; the moment one did it failed on runfiles realpathing out of the sandbox. Same for snapshots: `toMatchSnapshot()` asserted nothing at all, because the `.snap` was not in runfiles and vitest treated every run as a first run.
- **Emitting a target that cannot build is worse than emitting none, and silence is worse than both.** Gazelle names what it refused and the reason instead.
- **A config option another tool owns configures nothing.** `resolve.modules` is webpack's; Vite ignored it silently, so the dev server had no npm resolution at all and no test noticed, because no test imported an npm package from served source. A generated option is only real once something reads back the behaviour it was supposed to produce.
- **A `catch` that warns is how a feature becomes a no-op.** `react_refresh = True` reached into `@vitejs/plugin-react/dist/index.mjs`, a filename that major no longer shipped, and served without Fast Refresh behind a `console.warn`. Fail with the label and the fix, or do not catch.
- **The editor has one `paths` map.** A nested tsconfig extends the root and
  inherits its map unchanged (so the root's aliases still resolve from a
  subdirectory): "resolve this specifier" is a per-target fact on the build and
  a workspace-wide one in the editor. The map names first-party packages only;
  npm resolves through the checkout's `node_modules` in the editor and through
  the target's importer chain in the build, one resolver for both.
- **Silence in a metadata map is not an answer.** The entry reader the rules once had read `exports["."]` and stopped, so a string-valued entry with no `types` key (most of npm, and every `@types/*` package) resolved to nothing and a `paths` entry pointed at a directory. The reader is gone; tsgo reads the map in the store, and the lesson stands for any map a rule still reads.
- **A real version bump is a test.** Only moving `@npm` to Vite 8 / vitest 4 fired `test.workspace`, the react entry point and the declaration-entry fallback. Two hubs on two majors looked like coverage of exactly that and supplied none of it.
- **esbuild reads the workspace `tsconfig.json`.** `srcs` reach the sandbox as
  symlinks, so esbuild walks up from the entry point's real path, finds the
  source-tree `tsconfig.json`, and applies its `paths`; when those pointed npm
  names at `.d.ts` copies, a bundled npm package resolved to a declaration
  file. Every `esbuild_bundle` passes `--tsconfig-raw={}`; nothing noticed
  earlier because the vite plugin's single import is `--external`.
- **Formatting drift hides real drift.** For two rounds `bazel run //gazelle` could not be applied here, because ten fixtures differed from Gazelle's own rendering and nobody could tell those files from the ones it was actually changing. Keep the clean-tree diff empty so the next non-empty one means something.
- **Every hub name is the consumer's to claim.** `npm/extensions.bzl` gives the
  root module's `translate_lock` priority for every hub name, so none is
  privileged: a ruleset-internal target naming `@npm//:x` resolves into whatever
  lockfile the consumer registered, and the `dev_dependency` hubs do not exist
  for a consumer at all. `//vite:node_modules` named `@npm//:esbuild`,
  and it feeds `//vite:vite_plugin_bazel`, which `ts_dev_server` takes through
  its `plugin` attr. `plugin` has no default, and no workspace here
  set it, so nothing had reached the label and no build had failed. The `@npm`
  labels still in `//vite` are unreached, not sanctioned. One test pins the
  rule, `//tests/npm:vite_plugin_hub_test`, over the store `//vite:node_modules`
  links into.

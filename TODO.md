# rules_typescript — Remaining Work

## Project Vision

**"TypeScript on Bazel should feel like Go on Bazel."**

Today we have: compilation (oxc), type-checking and declaration emit (tsgo), npm
deps (pnpm lockfile), Gazelle, vitest testing and a dev server with HMR. See
the readiness table below per area.

What is still thin:

- Consumers build oxc from Rust source on first use, so cold start is nowhere
  near `rules_go`'s prebuilt-toolchain experience. This is the widest remaining
  gap against the vision above.
- A `vite_config` is loaded through Vite's own config loader after
  `ts_dev_server` stages it into bazel-bin with the local modules it imports
  (`vite_config_srcs`), so it resolves beside the Bazel npm tree rather than
  through a source-tree `node_modules`. What it still cannot carry is a *second*
  config file a framework wants beside it.

### Current Readiness

| Area | Status |
|------|--------|
| Compilation (oxc) | Production-ready |
| Type-checking (tsgo) | Production-ready; imports must be satisfied by a direct dep, checked per target |
| npm deps (pnpm → Bazel) | Production-ready; one repo per package, patches verified at extension time |
| node_modules trees | Every *resolution* placed — name, version and peer set (primary flat, the rest under `.pnpm/<name>@<version>[_<peer set>]/`, with a relative link per disagreeing edge) |
| Gazelle BUILD generation | Production-ready (JS/TS, path aliases from tsconfig.json); alias resolution is deterministic, extension-spelling specifiers resolve, and deps come from the tsgo listing the strict-deps check reads. CI pins four properties of a run over the `gazelle_roundtrip` workspace (the output builds; generating twice from scratch is byte-identical; the test-target set is unchanged; `bazel test //...` passes on the output) and, on this tree, that every test source file is claimed by a test target (`tools/ci/check_test_sources.sh`). A run on this tree is not a no-op: `bazel run //gazelle -- -mode=diff` exits 1; the BUILD files here are hand-written, and nothing pins them to Gazelle's output |
| Testing (vitest) | Solid (DOM run for real, coverage, the user's config, snapshots read; written by `vitest -u` in the package, watch mode, debugging) |
| Bundling | `ts_binary` takes any `BundlerInfo` bundler, in the CLI mode or the generated-Vite-config mode; the ruleset ships no implementation, so nothing in this tree exercises the bundle action |
| Dev server + HMR | Pluggable: `ts_dev_server(server = ...)` takes a `DevServerInfo`, oj 0.2.16 by default; Vite is optional. Serves first-party source with Bazel out of the inner loop. The Vite option resolves bare npm specifiers through the `node_modules` tree via the `bazel:npm-resolve` plugin; codegen rebuilds and config-aware restarts under ibazel; does not typecheck |
| IDE integration | Generated tsconfig + tsserver hook; `module_name` and `extra_exclude` supported. A package whose targets disagree with the root `compilerOptions` gets its own generated tsconfig, declared in `nested_tsconfigs` and staleness-tested; the root excludes those files individually so unclaimed ones stay in its program. Zero tsc errors across the root and all nine nested programs |
| CSS / assets | A `.css`, an image or a `.json` is a src of the `ts_compile` that imports it, staged beside the compiled `.js` and typed by the tsconfig (`vite/client`, a `declare module`, `resolveJsonModule`); a `*.module.css` is Vite's own CSS modules in the bundle and the dev server, and under vitest the class-name proxy `css.modules.classNameStrategy` shapes. Tailwind v4 works through `vite_config` under the dev server |
| CI/CD | Docs: remote caching (BuildBuddy/EngFlow), RBE, GitLab CI, non-determinism — documented, not exercised by this repo's CI |

### Known gaps, with the mechanism

Small enough not to need a sub-project, specific enough that nobody should have
to rediscover them. Each names the file to change.

- **`ts/private/tsconfig_aspect.bzl` pairs `@types/*` for direct deps only.**
  `ts_compile` reads the pairing for every package it names in `paths`, which is
  what makes an untyped package reached transitively (vitest → @vitest/expect →
  chai) resolve to its `@types/*`. The IDE tsconfig the aspect writes still walks
  direct deps, so the editor sees `chai` as untyped where the build does not.
- **Tailwind v4 scans from Vite's resolved `root`.** Under the dev server that
  is the workspace root, so the stylesheet needs no `@source` line; a bundler
  that sets another root has to name the files to scan. Prefer
  `@import "tailwindcss" source("<dir>")` to a bare `@source`: the former is
  validated and fails on a missing directory, the latter exits 0 and emits
  nothing. Scanning a compiled `.js` also loses a class name that only ever
  appeared in a type position. `//tests/tailwind:tailwind_dev_vite_test` covers
  the dev server.
- **Workers: tests run inside workerd.** `//tests/workers:worker_test` runs
  inside workerd against the `.js` Bazel compiled, via `SELF.fetch()`. The
  earlier diagnosis here -- that the pool must own the transform and cannot take
  compiled output -- was wrong; compiled `.js` is fine. Two things were missing:
  the pool has to be installed as a **Vite plugin** (`cloudflareTest()`, which
  both installs the pool runner and owns `cloudflare:test` through its own
  `resolveId`/`load`), not as `test.pool` (`cloudflarePool()` is only the runner);
  and a module must have **one identity** (ts_test's layer gives a package's
  file its realpath as its id), because the pool resolves modules for a second
  runtime and a lexical path there is a second module identity. Under
  `resolve.preserveSymlinks` it reads as
  `Cannot read properties of undefined (reading 'config')`, which looks like a
  plugin-API problem and is not.
  The deploy dry run that sat beside it (`ts_worker_dry_run_test`) went with
  M1's deletion of the wrangler rules; publishing stays a command a human or a
  release job runs outside the build.
  Also: `bazel coverage` now reports real per-line coverage for code executing
  inside workerd (`LH:4 LF:4` for `tests/workers/src/index.js`). The diagnosis
  recorded here before -- that instrumentation had to reach another runtime and a
  provider knob could not fix it -- was **wrong**. Two things were missing.
  `test.coverage.allowExternal` must be set: under Bazel every file under test is
  a build output whose realpath sits outside the vite root, so without it istanbul
  instruments nothing and writes an empty report while the run stays green. That
  is the `0 | 0 | 0 | 0` symptom, and removing the one line reproduces it on
  demand. And istanbul's `SF:` path is an eleven-level escaping relative path,
  so the report is not empty but wrong until `RewriteLcov` resolves it against
  the run directory. `coverageFlags` no longer hardcodes `--coverage.provider
  v8` (vitest defaults to v8 anyway), so a provider set in a config layer
  survives; `ts_test` gained a `coverage_provider` attr. Under Bazel 9.2.0 that
  report went nowhere: the launcher wrote `COVERAGE_OUTPUT_FILE`, which Bazel's
  `lcov_merger` then wrote from an empty `COVERAGE_DIR`, and that merger keeps a
  record only under the manifest's spelling, the `.ts`, where the report names
  the `.js`. The launcher writes `vitest.dat` under `COVERAGE_DIR`, its paths
  resolved against vitest's root (the config's package, not the runfiles
  directory `RewriteLcov` assumed), and `//tools/lcov_merger` is the rule's
  merger; `tools/ci/check_coverage_report.sh` runs `bazel coverage` on the
  fixture in the `test` job.
- **`ts_add_package` takes the hub whose lockfile it edits.** There is one
  `//:add_package_<hub>` per `npm.translate_lock()`, each pinned to that hub's
  `pnpm_lock`, because pnpm rewrites whichever lockfile it resolves against and
  which hub is being edited belongs in the command a person types. It no longer
  writes a `package.json` and `pnpm-lock.yaml` to the repository root. Remaining
  hole: a user-supplied `--lockfile-dir` still escapes the pinned hub — the
  wrapper appends `--dir` (last one wins) but constrains nothing else.
- **`//tests/npm/pnpm-lock.yaml` is a curated fixture that `pnpm` regenerates
  destructively.** Its comments say what each entry proves, and it carries a
  workspace link, a non-root importer, and a hand-placed tarball-prefix package
  that nothing depends on. A `pnpm add` against it dropped that package and every
  comment. New third-party dependencies belong in their own hub (`npm_tailwind`,
  `npm_eslint`, `npm_workers`) unless they are testing the translator itself.
- **The plugin licence is undecided, and the merge made it load-bearing.**
  `tools/isolated-declarations-lint/` was merged into `eslint-plugin/` and
  deleted. The surviving package declares `Apache-2.0`; the code ported into it
  came from an `MIT`-declared package, and the repository's only licence file
  (`/LICENSE`) is MIT. No Apache-2.0 text exists in the tree, and
  `files: ["dist", "README.md"]` ships neither a licence nor a README (there is
  no `eslint-plugin/README.md`). Pick one licence, add the matching text, and put
  it in `files` before publishing.
- **The Gazelle test-set property is now a CI step.**
  `tools/ci/check_test_sources.sh` asserts every tracked test source on disk is
  named in some test target's `srcs`, in two loading-phase queries (folded into
  the existing `test` job). It is anchored to something Gazelle does not
  write, which is why a run that *deletes* a test target cannot satisfy it — the
  failure mode that lost seven `go_test` targets in an earlier round. Verified
  red by deleting a `go_test` and a `ts_test` block, then restored.
  A `manual` tag is checked too: a file whose every claiming target is `manual`
  must be in the script's `MANUAL_ONLY` allowlist with a reason, and the list is
  exact in both directions; today's three are
  `tests/node_test/analysis/attrs.test.ts`,
  `tests/workers_nested/test/data_shadow.test.ts` and
  `tests/vitest/reads_report/reads_report.test.ts`.
  Two properties are still hand-verified: `bazel test` on what Gazelle wrote, and
  the roundtrip test's comparison is scoped to a synthetic 3-package child
  workspace with no Go.

- **Inside this repository Gazelle emits `load("@rules_typescript//ts:defs.bzl",
  …)`,** the external label, which resolves through the module's self-mapping but
  is not what a maintainer writes by hand — so BUILD files here carry both forms.
  Cosmetic, and it costs a reader a moment every time.
- **Gazelle keeps emitting the external `@rules_typescript//` load label inside
  this repository.** A per-run "generating for self" flag was tried and reverted:
  `Loads()` has no directory context, so one flag decides for the whole walk --
  and `e2e/basic`, `examples/*` and the staged workspaces under
  `tests/integration/` are separate modules whose BUILD files carry these loads
  and must keep the external form. It also emitted a second, differently-labelled
  load of the same `.bzl` into any un-normalised file it touched. Cosmetic gain,
  real regression; it needs a per-directory hook that does not exist.
- **The consumer audit found no conflict to absorb, so nothing was absorbed.**
  A tree with 16 `ts_compile` targets across 11 packages has **3** multi-target
  packages and **0** conflicts — not one target sets any compiler option at all,
  and Gazelle never emits an options attr, so a same-package conflict can only be
  hand-authored. (The ruleset's own tree: 33 multi-target packages, 0 conflicts.)
  Two defects found while reading the mechanism *were* worth fixing and are:
  option values are canonicalised before comparison (TypeScript reads `target`,
  `module`, `moduleResolution`, `jsx`, `moduleDetection`, `newLine` and `lib`
  case-insensitively, and `lib` as a set — `"ES2022"` vs `"es2022"` was a hard
  error), and `target`/`jsx_mode` now reach the editor. Those two stay rule attrs
  and never appear in `compiler_options_json`, so a package compiled as `es2017`
  or `jsx_mode = "preserve"` was silently checked against the root's
  ES2022/react-jsx — exactly the divergence nested configs exist to end. Pinned by
  `//tests/compiler_options/es_target`. Both sides of the equality have to be
  canonical: canonicalising only the group's side gives every package a file it
  does not need.
- **Differing `extends` merge silently; differing keys are an error.**
  `_nested_configs` unions `group.extends` into an array while a same-key
  disagreement fails analysis. `vite/tsconfig.json` shows the effect —
  `extends: ["../tsconfig.json", "./vite.tsconfig.json"]` over an include list
  containing a file whose target does not extend that baseline in the build. Same
  guarantee, opposite treatment. Either conflict-check non-equal non-empty
  extends sets the way keys are checked, or document that baselines merge.
- **The nested-config grouping path has no test.** Neither the `fail()` on a
  same-package conflict nor the merging in `_nested_configs` is covered;
  `tests/lsp/ide_tsconfig_tests.bzl` covers ambient types and module paths only.
  An `analysistest` with `expect_failure` on a two-target fixture would pin it —
  the fixture must not be tagged `manual`, since `_option_group` skips those.
- **Nested configs are emitted per package, not per source directory.**
  `dest = group.package + "/tsconfig.json"`. Where a package's targets have
  disjoint source directories, emitting at the deepest common source directory
  would give each its own program and dissolve that class of conflict, since
  tsserver walks up from the file and does not care about package boundaries.
  Where targets share a directory there is genuinely no representation and the
  error is right. Blocked on that `dest` and on `ts_refresh_tsconfig`'s per-nested
  `diff_test`, which assumes a nested config's directory is itself a Bazel
  package. Not worth building until a real conflict exists.
- **`_short_digest` is Java's `String.hashCode` masked to 32 bits.** Two peer
  suffixes agreeing on their first 40 sanitised characters *and* colliding on
  that hash merge into one repository, and now also one store directory. A
  pre-existing bound, not widened by the resolution keying (the store key is the
  same token), and not eliminated: a real hash function is not available in
  Starlark.
- **The `node_modules` tree action resolves node through `js_runtime_type` (the
  target platform) rather than `js_tool_type` (the exec platform).** Every other
  build action was moved. Harmless while exec == target, wrong under
  cross-compilation.

---

## Sub-Project 1: Bundlers

**Goal:** `ts_binary(bundler = ...)` runs any bundler returning `BundlerInfo`.

The Vite bundler and the bundle rule were deleted: the consumer this ruleset is
built for never instantiated either, and a bundler nothing runs is a maintenance
cost with no test to defend it. `ts_binary` keeps the `BundlerInfo` seam in both
invocation modes, so a bundler is a rule returning the provider.

### 1.1 Alternative Bundlers
- [x] Document the `BundlerInfo` interface for custom bundler authors (docs/guides/bundling.md)
- [ ] Create esbuild bundler implementation (for speed-focused users)
- [ ] Create Rolldown bundler implementation (when Rolldown stabilizes)

---

## Sub-Project 2: Dev Server & HMR

**Goal:** `bazel run //app:dev` starts the default oj dev server with HMR; Vite remains optional. Edit a `.ts` file, see changes in the browser within 500ms.

### 2.1 ts_dev_server Rule
- [x] Create `ts/private/ts_dev_server.bzl` as an executable rule
- [x] Accept `entry_point` (ts_compile target)
- [x] Accept `port`, `host`, `open` attrs
- [x] Accept optional `plugin` attr (compiled vite-plugin-bazel .mjs)
- [x] Generate runner script that starts Vite dev server
- [x] Wire runfiles: compiled .js files, node_modules tree
- [x] Export from `ts/defs.bzl`
- [x] Accept `server` attr (DevServerInfo provider, for custom dev servers)

### 2.2 Vite Plugin — Dev Mode
- [x] Build `vite/src/*.ts` inside Bazel (genrule using esbuild — `//vite:vite_plugin_bazel`)
- [x] `bazelPlugin()` in plugin.ts resolves `.ts` imports to compiled `.js` in bazel-bin
- [x] `BazelWatcher` in watcher.ts watches bazel-bin for changes (triggered by ibazel)
- [x] HMR invalidation on file change (`handleRebuild` in plugin.ts)
- [x] Wire compiled plugin into ts_dev_server via `plugin` attr
- [x] Generate Vite config for dev mode that dynamically imports the plugin
- [ ] Handle `rootDirs`-style path mapping (source tree ↔ output tree) — BazelResolver partially handles this
- [x] Support React Fast Refresh (via `@vitejs/plugin-react`): `react_refresh = True` attr on `ts_dev_server`. The entry point comes from the package's own `exports` map, and a load failure throws naming the label and the dep to add -- it used to reach into `dist/index.mjs`, which the installed major does not ship, and warn
- [x] Resolve bare npm specifiers out of dev-served source: the `bazel:npm-resolve` `pre` plugin locates the package in the `node_modules` tree and hands the id back to Vite's own resolver, so exports maps, conditions and subpaths stay Vite's. `resolve.modules` never did this -- it is a webpack option Vite ignores
- [x] Load `vite_config` from a copy in bin, so its own bare imports resolve beside the Bazel npm tree rather than in the source tree (`//tests/dev_server:vite_config_boundary_test`)

### 2.3 ibazel Integration
- [x] Document the `ibazel run //app:dev` workflow in ts_dev_server.bzl docstring
- [x] Vite's file watcher monitors bazel-bin for .js changes (no server restart needed)
- [x] Runner script passes BAZEL_BIN_DIR env var to the Vite config
- [x] Decide restart-or-keep inside the Vite process instead of via ibazel's protocol: the launcher survives ibazel's SIGTERM, and `ConfigWatcher` compares content digests of the inputs the generated config was built from
- [x] Trigger Vite HMR update on ibazel rebuild completion (`BazelWatcher` in plugin.ts, for `bazel-bin` outputs)
- [x] Handle incremental rebuilds (only changed modules invalidated)
- [x] Take Bazel out of the inner loop entirely for first-party source: Vite transforms checked-in source in memory, so no Bazel cycle sits between a save and the browser
- [ ] Detect ibazel via `IBAZEL_NOTIFY_CHANGES` (not needed by the design above; would only add an explicit protocol)
- [ ] Commit a benchmark for edit-to-HMR latency — the loop is measured by hand today, nothing pins it

### 2.4 Gazelle Integration
- Gazelle generates `ts_dev_server` for application entries, updates its managed attributes and removes stale generated targets. It preserves server choices and `# keep` values. It requires non-test program sources and a package index.html or listed main.ts[x]/app.ts[x].

---

## Sub-Project 3: CSS & Asset Support

**Goal:** `import "./Button.css"` works in compilation, bundling, and dev server. Assets (images, fonts, SVGs) are handled correctly.

### 3.1 CSS Imports in Compilation
- [x] `ts_compile` accepts every file in `srcs`; a `.css`, an image or a `.json` is staged beside the `.js` and carried in `TsInfo.transitive_data`
- [ ] Strip CSS import statements from compiled `.js` — the bundler (Vite) handles this at bundle time; for library targets without a bundler, oxc leaves CSS imports in the .js output which may cause runtime errors if executed directly in Node.js without a bundler

### 3.3 Tailwind CSS
- [ ] Support `@tailwind` directives
- [ ] Wire Tailwind as a PostCSS plugin in the bundler
- [ ] Content scanning for purging unused styles

### 3.4 Asset Handling
- [ ] Asset hashing for cache busting in production bundles
- [ ] Asset manifest generation
- [ ] Copy assets to bundle output directory

---

## Sub-Project 4: Framework Integration

Deleted. The framework build rules, the Vite bundler they built on, Gazelle's
framework detection and its bundle writers had no consumer and went with M1 of
the migration. A framework's Vite plugin runs in the dev
server through `vite_config`; nothing runs a framework's own build. Reopening
this is a design question, not a checklist.

---

## Sub-Project 5: Testing Maturity

**Goal:** vitest tests work reliably with DOM testing, coverage, snapshots, and custom config.

### 5.0 ts_test Ergonomics (DONE)
- [x] The rule builds the node_modules forest from the @npm// deps; there is no node_modules target or attribute
- [x] Gazelle no longer generates node_modules rules; emits empty stubs to delete stale ones
- [x] `# gazelle:ts_runtime_dep`: Gazelle appends listed labels to every ts_test deps list — eliminates manual happy-dom, react, @vitest/coverage-v8 additions

### 5.1 DOM Testing
- [x] Verify @testing-library/react works with vitest in Bazel sandbox
- [x] `test.environment` is the config file's, as under plain `vitest`: //tests/setup_files_compiled/dom runs under happy-dom, and //tests/vitest/attrs sets `node` and `globals` and runs on what the file set. Bazel's layer gives a module the runfiles hold its runfiles path as its id and serves `bazel-bin` too: a DOM environment loads every module through Vite's server. happy-dom is in the test lockfile; jsdom and edge-runtime are not, and nothing pins them
- [x] Create example with @testing-library component tests

### 5.2 Coverage
- [x] Pass --coverage flag to vitest CLI
- [x] Collect coverage artifacts and integrate with bazel coverage (`COVERAGE_DIR` + the rule's `_lcov_merger`, `//tools/lcov_merger`)
- [x] Collect coverage artifacts (lcov) as test outputs (the launcher writes `vitest.dat` under `COVERAGE_DIR`; the merger writes `coverage.dat`)
- [x] Integrate with Bazel's `--combined_report=lcov` (combined report produced at `bazel-out/_coverage/_coverage_report.dat`)
- [x] Support `--instrumentation_filter` for selective coverage (every `ts_compile` carries `InstrumentedFilesInfo`, the merger keeps what the manifest selects; tests/vitest/coverage pins the selection)

### 5.3 Snapshot Testing
- [x] Solve the read-only sandbox for snapshot writes. `test.resolveSnapshotPath` points at `<package>/__snapshots__/<source>.snap`; the `.snap` is a src, so a stale or missing one FAILS instead of being rewritten in the sandbox; `CI=true` keeps `bazel test` read-only; writing is `vitest -u` in the package. `--sandbox_writable_path` is no longer involved.
- [x] Document the snapshot workflow in a Bazel context (docs/rules/ts-test.md, docs/guides/testing.md)
- [x] Test that can fail: //tests/vitest/snapshot reads its checked-in `.snap` from `srcs`

### 5.4 Custom vitest Configuration
- [x] Add `config` attr to `ts_test` (label to vitest.config.ts)
- [x] Reporters, setup files, global setup: the `config` file's, as under plain `vitest`
- [x] Support an array-form `config` for monorepo configurations. It becomes `test.projects` -- the name vitest 3.2 renamed `test.workspace` to and vitest 4 removed the old spelling of -- and each project gets the Bazel layer

### 5.5 Watch Mode
- [x] Document `ibazel test //path:test` as the watch mode workflow (README.md)
- No custom rule needed: ibazel works with standard `ts_test` targets out of the box.

### 5.6 Debugging
- [x] Document how to attach a debugger to vitest in Bazel (README.md)
- [x] `--inspect-brk` documented via `env = {"NODE_OPTIONS": "--inspect-brk=9229"}` pattern
- [x] VS Code launch.json template created (.vscode/launch.json.template)

---

## Sub-Project 6: npm Support Hardening

**Goal:** Handle real-world npm dependency graphs (100+ packages, multiple versions, bin scripts, workspaces).

### 6.1 Bin Scripts
- [x] Extract `bin` field from package.json during npm_translate_lock
- [x] Generate executable targets for each bin script (e.g., `@npm//:vitest_bin`)
- [x] Wire bin scripts into `ts_test` vitest resolution (replace heuristic path)
- [x] Support `npx`-style invocation: `bazel run @npm//:tsx -- script.ts`

### 6.2 Multiple Package Versions
- [x] Support pnpm's package aliasing (`react@18` + `react@19` in same lockfile)
- [x] Generate versioned target names: `@npm//:react_19_1_0` alongside `@npm//:react`
- [x] Primary alias (`@npm//:react`) points to highest semver version (preserved behaviour)
- [x] Dependency edges from dependents correctly reference the versioned label they actually use
- [x] Test: `@rolldown/pluginutils` at 1.0.0-rc.3+1.0.1 is exercised by the existing lockfile (//tests/npm:npm_multi_version_test)
- [x] Resolve the correct version per dependent in a `node_modules` tree: `NpmPackageInfo.direct_deps` carries the per-dependent resolution, the primary version stays flat and every other version gets a store directory plus a link from the dependent that resolved to it
- [x] Carry pnpm's peer suffix, so two snapshots sharing `name@version` but differing in injected peers stop collapsing onto one directory: `NpmPackageInfo.peer_id` reaches the layout planner from the snapshot key, the non-primary resolution gets `.pnpm/<name>@<version>_<peer set>/node_modules/<name>`, and declaring two of them on one target is an error

### 6.3 pnpm Workspaces
- [x] Parse `pnpm-workspace.yaml`
- [x] Resolve `workspace:*` protocol references to local Bazel targets (see 13.1)
- [x] Map workspace packages to ts_compile targets — the hub holds an `alias` to the local target
- [ ] Make a workspace member importable by its package name without a hand-written `module_name`: Bazel resolves the hub alias before Starlark sees it, so the imported name survives only in the alias

### 6.4 Conditional Exports
- [x] Parse `exports` field in package.json
- [x] Resolve conditional exports (import/require/types/default) correctly
- [x] Wire resolved entry points into the declaration provider (since replaced: tsgo resolves entries through the forest)

### 6.5 Integrity & Security
- [ ] Verify SRI hashes for all downloaded packages (fail if missing, with override)
- [ ] Add `--strict_npm_integrity` flag
- [ ] Support npm audit integration (report vulnerable packages)

### 6.6 Performance
- [x] Lazy package download (one repository per package; Bazel fetches a package the first time a target needs it)
- [x] Parallel tarball downloads (independent repositories fetch in parallel)
- [x] Cache downloaded tarballs across `bazel clean` (the repository cache, not something the ruleset implements)

---

## Sub-Project 7: Gazelle Improvements

**Goal:** Gazelle handles real-world TypeScript patterns including CSS, dynamic imports and path aliases.

### 7.2 Dynamic Import Handling
- [x] Detect `import("./page")` dynamic imports
- [x] Generate deps for dynamically imported modules
- [x] Support template literal dynamic imports: `` import(`./pages/${name}`) `` (skip, don't error)

### 7.3 Path Alias Reading from tsconfig.json
- [x] Read `compilerOptions.paths` from tsconfig.json
- [x] Support `baseUrl` + `paths` resolution

### 7.4 Re-export Handling
- [x] `export * from "./utils"` should resolve to the re-exported module
- [x] `export { foo } from "./bar"` should add dep on `./bar`'s package
- [x] Handle barrel files (index.ts that re-exports everything)

### 7.5 Error Reporting
- [x] Warn when an import cannot be resolved (instead of silently dropping)
- [x] Show which resolution strategies were tried
- [x] `# gazelle:ts_warn_unresolved` directive to control warning level

### 7.6 Generated File Patterns
- [x] Exclude `.next/`, `.nuxt/`, `.svelte-kit/`, `dist/`, `build/` directories
- [x] Configurable exclude patterns via `# gazelle:ts_exclude`
- [x] Handle `*.gen.ts`, `*.generated.ts`, `*.auto.ts` patterns

---

## Sub-Project 8: Isolated Declarations Migration

**Goal:** Provide tooling that helps teams add explicit return types to all exports, enabling isolated declarations.

### 8.1 ESLint Plugin
- [x] Implement `@rules_typescript/eslint-plugin-isolated-declarations`
- [x] Rule: `require-explicit-types` — error on exports without explicit return type
- [ ] Auto-fix: infer return type from TypeScript and insert annotation
- [x] Handle edge cases: overloads, generics, conditional types
- [x] Publish to npm (package prepared with dist/ built; not actually published)

### 8.2 Migration CLI
- [ ] Create `isolated-declarations-migrate` CLI tool
- [ ] Scan codebase for exports missing explicit types
- [ ] Report count and locations
- [ ] `--fix` mode: auto-insert inferred types (requires tsc or tsgo for inference)
- [ ] `--check` mode: exit 1 if any violations (for CI)

### 8.3 Gradual Rollout
- [x] Support the emitter choice per target: `declarations = "tsgo"` (default, no annotations needed) or `"oxc"` (isolated declarations). `isolated_declarations` and `ts_compile_legacy` are deleted
- [x] Document migration strategy: enable per-package, fix violations, move to next package

---

## Sub-Project 9: Developer Experience

**Goal:** Using rules_typescript feels seamless — IDE integration, clear errors, fast feedback loops.

### 9.1 IDE Integration
- [x] Generate tsconfig.json at workspace root for IDE consumption
  - [x] `bazel run //:refresh_tsconfig` target, `deps`-driven, with a staleness diff test under `test = True`
  - [x] Maps Bazel package structure to tsconfig `paths`, including one entry per `module_name` and one per npm package (installed under `npm_dir`)
  - [x] Points at bazel-bin for .d.ts resolution via rootDirs
  - [x] `extra_exclude` keeps TypeScript outside this module's graph out of the program
- [x] Per-target compiler options: a package whose targets disagree with the root `compilerOptions` gets its own generated tsconfig, listed in `nested_tsconfigs` and staleness-tested; the root excludes those files one by one. Two targets in *one* package still cannot disagree — tsserver picks a program by directory — and that remains structural
- [x] VS Code settings template (.vscode/settings.json.template)
- [x] IDE setup documented as Quick Start step 5 (right after Gazelle)
- [ ] WebStorm/IntelliJ configuration guide

### 9.2 Error Messages
- [x] Audit all `fail()` calls — ensured each has actionable guidance
- [x] Added `Did you mean...?` suggestions for common mistakes (ts_binary, node_modules)
- [x] `build --output_groups=+_validation` in all .bazelrc files: type errors now fail `bazel build` by default
- [ ] Improve oxc error output for isolated declarations failures

### 9.3 Build Feedback
- [x] Add `--show_result=N` recommendation to docs (README.md)
- [ ] Create `bazel_ts_info` rule that reports compilation statistics
- [ ] Consider progress messages in actions ("Compiling 5 TypeScript files...")

### 9.4 Linting Integration
- [x] `ts.lint(binary, config, fail_on_warnings)` in the root MODULE.bazel names the linter once
- [x] Every `ts_compile` and `ts_test` runs it as a validation action (like type-checking)

---

## Sub-Project 10: Production & CI/CD

**Goal:** rules_typescript works reliably in CI/CD pipelines with remote caching and execution.

### 10.1 Remote Caching
- [x] Document setup with BuildBuddy, EngFlow, or Bazel Cache
- [x] Verify all actions are hermetic (no network access, no env leaks)
- [ ] Test with `--remote_cache` flag (requires external infra)

### 10.2 Remote Execution
- [x] Document RBE setup (BuildBuddy RBE, EngFlow, custom executor image)
- [x] Verify oxc-bazel, tsgo, and Node binaries work in remote execution (statically linked)
- [x] Platform-specific binary selection via exec platform constraints (documented)
- [ ] Test with `--remote_executor` flag (requires external infra)

### 10.3 CI Examples
- [x] GitHub Actions workflow template
- [x] GitLab CI template
- [x] Generic CI steps documented (docs/CI_CD.md). `scripts/ci.sh` is deleted: it duplicated the workflow

### 10.4 BCR Publishing
- [x] `.bcr/metadata.json` owns the maintainer identity. It is the single source of truth: the release
  workflow's notes step reads it (`jq -r '.maintainers[0]…'` in
  `.github/workflows/publish-to-bcr.yml`) rather than carrying its own copy.
- [x] Automate `source.json` integrity hash on release
- [x] Release tool that bumps, commits and tags (`bazel run //tools/release`); the tarball and integrity hash come from the release workflow, since a locally built archive is not the published one
- [ ] Submit to BCR

### 10.5 Determinism
- [x] Verify builds are bit-for-bit reproducible
- [x] Two builds diffed byte for byte — the `determinism` CI job, since two builds cannot be one Bazel action
- [x] Document any known sources of non-determinism (docs/CI_CD.md)

---

## Sub-Project 11: Platform Support

**Goal:** rules_typescript works on all major platforms.

### 11.1 Windows

**Foundation (done):**
- [x] Add `windows_amd64` to `_NODE_PLATFORM_CONSTRAINTS` and `_NODEJS_REPO_PLATFORM` in `ts/private/runtime.bzl`
- [x] Add `nodejs_windows_amd64` to `use_repo` and `register_toolchains` in `MODULE.bazel`
- [x] Replace bash script in `node_modules` rule with cross-platform Node.js builder (`_BUILDER_MJS_CONTENT` in `ts/private/node_modules.bzl`):
  - Node.js `copyFileSync` + `mkdirSync` replace bash `cp` + `mkdir -p`
  - Works on Windows, Linux, and macOS identically
  - Falls back to bash script when JS runtime toolchain is not registered (POSIX only)
  - Comment fixed: paths are exec-root-relative (Bazel cwd = exec root during actions), not absolute
- [x] Eliminate duplicate bash script in `_ts_auto_node_modules` (used by `ts_test` macro):
  - Now delegates to `build_node_modules_action` from `node_modules.bzl`
  - Added `toolchains` attr to `_ts_auto_node_modules` rule
  - Made `mandatory = True`: rule is only used inside `ts_test` where Node is always available; prevents silent fallback on misconfigured setups
- [x] Export `build_node_modules_action` as a public helper so both `node_modules` and `ts_test` share the same cross-platform copy logic
- [x] Document Windows limitation clearly at the top of `ts_test.bzl`: the node_modules tree action is cross-platform, but the bash runner scripts are not

**Runners (done):**
- [x] Replace the bash runner scripts in `ts_test.bzl`, `ts_binary.bzl`, `ts_dev_server.bzl` and `npm_bin.bzl` with one checked-in Go launcher (`//tools/launcher`) reading a per-target JSON config. The generated file is `<target>_launcher`; `--dump-config` prints what it resolved

**Remaining for full Windows support:**
- [ ] Replace the remaining bash *action* wrapper: the `node_modules` fallback taken when no JS runtime toolchain is registered
- [ ] Windows path handling in whatever replaces them (backslash vs forward slash)
- [ ] Build oxc-bazel for Windows (x86_64, arm64) via rules_rs cross-compilation or pre-built binaries
- [ ] Verify tsgo Windows binaries exist in the `@typescript/native-preview` npm packages
- [ ] Add `windows_amd64` to `PLATFORM_CONSTRAINTS` in `ts/private/toolchain.bzl` (needed for oxc and tsgo toolchains)
- [ ] Test on Windows CI (GitHub Actions `windows-latest` runner)
- [ ] Upstream, and blocking regardless of the above: no tsgo binary and no hermetic pnpm binary are published for Windows

### 11.2 Linux ARM64
- [x] Build oxc-bazel for linux-aarch64 (built from source via rules_rs — no pre-built binary needed)
- [x] Verify tsgo linux-arm64 npm package exists (@typescript/native-preview-linux-arm64 at 7.0.0-dev.20260311.1)
- [x] Add to `PLATFORM_CONSTRAINTS` (both oxc and tsgo; sha256 checksum verified and added)
- [ ] Test on ARM64 CI (GitHub Actions ARM runner or self-hosted)

### 11.3 Container Builds
- [ ] Provide Dockerfile with all dependencies pre-installed
- [x] Document Bazel-in-Docker workflow (README: Platform Support > Container Builds)
- [x] Verify sandbox works in Docker (no privileged mode needed — documented in README)

---

## Sub-Project 12: Monorepo & Package Publishing

**Goal:** Support large monorepos with internal packages and npm-publishable packages.

### 12.1 Internal Packages
- [x] Document recommended monorepo layout (README: Monorepo Layout section)
- [x] Gazelle auto-detects packages from directory structure (existing feature)
- [x] Support `# gazelle:ts_package_boundary` for explicit boundaries (existing feature)

### 12.2 Publishable Packages
The npm publish rule was deleted with M1 of the migration: no consumer
instantiated it. Publishing is out of scope until one does.

### 12.3 Workspace References
- [ ] Parse `pnpm-workspace.yaml`
- [ ] Map `workspace:*` references to Bazel targets
- [ ] Gazelle resolves workspace package imports to Bazel labels

---

## Sub-Project 13: Path to A-Rating (rules_go parity)

**Goal:** Eliminate every friction point that prevents a TypeScript team from adopting rules_typescript as confidently as a Go team adopts rules_go.

### 13.1 pnpm Workspace Support
- [x] Parse `pnpm-workspace.yaml` in `npm_translate_lock` repository rule
- [x] Detect `workspace:*` protocol version specs in lockfile `importers` section (pnpm v6 inline and v9 block formats)
- [x] Map workspace packages to Bazel labels: `workspace:*` → `//packages/shared:shared`
- [x] Generate `alias` targets in `@npm` repo for workspace packages pointing at local Bazel targets (`@@//path:name` canonical prefix)
- [ ] Gazelle resolves `import { Foo } from "@myorg/shared"` to `//packages/shared` when the package is a workspace member
- [x] Test: `@npm//:shared` alias resolves to `@@//packages/shared:shared` (//tests/npm:workspace_consumer)

### 13.2 Invisible node_modules Naming
- [x] `node_modules()` rule uses `ctx.label.name` as output directory name, enabling multiple targets per package

### 13.7 vite/client Types Automatically Available
- [x] Created `ts/vite_env.d.ts` standalone shim (no vite npm dep needed) with:
  - `ImportMetaEnv` interface (MODE, BASE_URL, PROD, DEV, SSR, [key: string])
  - `ImportMeta.env` + `ImportMeta.hot` for HMR
  - Asset URL module declarations (*.svg, *.png, *.jpg, etc.)
  - CSS module declarations (*.module.css, *.module.scss, etc.)
- [x] Added `vite_types` bool attr to `ts_compile` macro that auto-prepends `@rules_typescript//ts:vite_env.d.ts`
- [x] Exported via `exports_files(["vite_env.d.ts"])` in `ts/BUILD.bazel`

### 13.8 Coverage with bazel coverage
- [x] Configure vitest to write lcov report to a known path (under `COVERAGE_DIR`, set by `bazel coverage`)
- [x] Wire `_lcov_merger` tool for `bazel coverage --combined_report=lcov` (`_lcov_merger = //tools/lcov_merger`)
- [x] The coverage output is collected as a test output and available in `bazel-testlogs`
- [x] Test: `bazel coverage //tests/vitest/coverage:math_coverage_test --combined_report=lcov` produces lcov file at `bazel-out/_coverage/_coverage_report.dat`
- [x] Requires `@vitest/coverage-v8` in npm deps; documented in tests/vitest/coverage/BUILD.bazel
- [x] node_modules symlink created at RUNFILES root so Vite can resolve `@vitest/coverage-v8` in sandbox
- [x] lcov paths normalized (`SF:_main/` prefix stripped by `RewriteLcov`) before writing `vitest.dat`

### 13.9 Zero-Prerequisites First Run
- [x] Document EXACT steps from empty directory to passing build (including Bazelisk install) — see README Requirements section
- [x] Ensure first `bazel build` works without any pre-installed tools (no pnpm, no node, no go — all fetched by Bazel)
- [x] The only prerequisite is Bazelisk (or Bazel 9+)
- [x] Test: fresh `MODULE.bazel` + source files → `bazel build //...` succeeds (`bazel run //tools/quickstart`)
- [x] `bazel run //tools/quickstart` builds a minimal workspace from scratch in a temp dir and verifies compilation and type-checking

---

## Priority & Sequencing

### Phase A — Immediately Useful (weeks)
Sub-projects that make the existing system more robust:
1. **SP5: Testing maturity** (5.1 DOM testing, 5.4 custom config)
2. **SP6: npm hardening** (6.1 bin scripts, 6.4 conditional exports)
3. **SP7: Gazelle improvements** (7.1 CSS recognition, 7.5 error reporting)
4. **SP8: Migration tooling** (8.1 ESLint plugin)

### Phase B — Core Value (months)
Sub-projects that unlock real application support:
6. **SP3: CSS support** (3.1-3.2 CSS imports and modules)
7. **SP2: Dev server** (2.1-2.3 ts_dev_server with HMR — done, minus a committed latency benchmark)

### Phase C — Developer Experience (months)
9. **SP9: Developer experience** (9.1 IDE integration)

### Phase D — Scale & Polish (ongoing)
10. **SP10: CI/CD** (10.1-10.4)
11. **SP11: Platform support** (11.1 Windows)
12. **SP12: Monorepo patterns** (12.2 publishable packages)

---

## Honest Assessment

**What works today:**
- Pure TypeScript library monorepo with npm deps, vitest tests, hermetic builds. Good for backend services, shared libraries, CLI tools.
- CSS and asset support: a `.css`, an image or a `.json` is a src of the `ts_compile` that imports it, typed by the tsconfig and staged beside the compiled `.js`.
- Gazelle: generates ts_compile and ts_test targets from TypeScript source files. Reads path aliases from tsconfig.json compilerOptions.paths/baseUrl.
- Dev server: ts_dev_server defaults to oj 0.2.16; the optional Vite server serves first-party source with Bazel out of the inner loop; bazel-bin supplies codegen output, assets and the npm tree. Under ibazel one Vite process lives across rebuilds and restarts only when the config's own inputs change. It does not typecheck, which is native Vite parity but makes the editor load-bearing. The server attr accepts DevServerInfo for custom dev server implementations. With the Vite server, react_refresh = True wires @vitejs/plugin-react for React Fast Refresh.
- CI/CD: documented remote caching (BuildBuddy/EngFlow/self-hosted), remote execution, GitLab CI template, and known sources of non-determinism. Documented, not exercised: this repository's own CI configures no remote or disk cache.

**What doesn't work today:** production bundling and framework build
pipelines. `ts_binary` takes a `BundlerInfo` bundler and the ruleset ships
none; a framework's Vite plugin runs in the dev server through `vite_config`,
and nothing runs a framework's own build.

**Effort estimate:** Sub-project 2 HMR (ibazel protocol, React Fast Refresh, <500ms latency) is another 1-2 months. Full feature parity with the JavaScript ecosystem is a multi-year effort.

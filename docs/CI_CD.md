# CI/CD & Production Readiness

The pipeline this repository runs, and the release path out of it. The remote
cache and remote execution sections further down describe configurations for a
consumer workspace, which this repository's CI does not run.

## GitHub Actions CI

`.github/workflows/ci.yml` runs on every push to `main`/`develop` and on every
pull request, whichever branch it targets. It has six jobs.

The five jobs that run Bazel set it up with `bazel-contrib/setup-bazel@0.18.0`:
bazelisk and repository caches in all of them, the external cache in all but
`examples`, and a disk cache where the job wants one.

### Workflow Jobs

1. **Unit Tests & Type Checking** (`test`)
   - On ubuntu only, first: `tools/ci/check_test_sources.sh`, which requires
     every tracked test source to be claimed by a test target that runs. It is
     a loading-phase query the next step pays for anyway
     (see [below](#test-source-coverage))
   - On ubuntu only, second: `tools/ci/check_integration_shards.sh`. Every
     integration test has to land on exactly one leg of the `integration-tests`
     matrix
   - On ubuntu only, third: `tools/ci/check_retired_names.sh`. No tracked
     prose or code outside the changelog names a retired attribute, kind,
     provider, directive, export or path (see [below](#retired-names)). The
     three gates run before the suite and none skips it: the two `bazel` steps
     run whatever the gates did, and the job still fails on a failed gate
   - `bazel test --config=ci //...`, then
     `bazel build --config=ci //... --output_groups=+_validation`, then, on
     ubuntu only, `tools/ci/check_coverage_report.sh`: the fixture under
     `bazel coverage`, its report's `SF:` lines against what the filter selects
     (see [below](#coverage-report))
   - Matrix: `ubuntu-latest` and `macos-latest`

2. **E2E Tests** (`e2e`)
   - `bazel run //:gazelle -- -mode=diff` in `e2e/basic`, a separate
     workspace, after `bazel run //:pnpm -- install --frozen-lockfile`: its
     BUILD files are what Gazelle writes; then builds and tests it
   - Matrix: `ubuntu-latest` and `macos-latest`

3. **Examples Build** (`examples`)
   - One matrix leg per workspace under `examples/` (`basic`, `app`,
     `react-app`), each a separate Bazel invocation, `fail-fast: false`. Every
     leg checks `bazel run //:gazelle -- -mode=diff` prints nothing (after an
     install where a root `pnpm-lock.yaml` is), then builds `//...`
   - All three legs share one disk cache key (`disk-cache: examples`). Most of
     each example's actions are the same oxc/Rust and toolchain prefix, and
     three keys would not fit GitHub's 10 GB cache limit

4. **Build Determinism Check** (`determinism`)
   - `tools/ci/check_determinism.sh /mnt/rules_ts_det`: `//tests/smoke:hello`
     and the four tools built from two empty output bases, then every file of
     the built configuration compared byte for byte. Two builds cannot be one
     action, so this check stays a script of invocations
   - The script disables disk and remote caches and remote execution: a hit
     on the second build would compare it against a copy of the first
   - Scratch space is `/mnt/rules_ts_det`, provisioned per run. `/mnt` is a
     directory on the root filesystem, not a separate disk, so the two full
     toolchain trees do not change volume; both jobs log `df -h /mnt /`. The
     separate directory keeps them out of the checkout and makes their size
     visible to `df`

5. **Integration Tests (nested Bazel)** (`integration-tests`)
   - Two legs, one per shard (`npm`, `core`), each running
     `bazelisk test --config=ci-integration-<shard> //tests/integration/... --test_env=RULES_TS_IT_SCRATCH=/mnt/rules_ts_it`.
     Each config in `.bazelrc` selects tests by the `shard-<name>` tag
     `nested_bazel_tags(shard = ...)` in `tests/integration/tags.bzl` adds;
     `core` is the complement of the other, so a test with no shard tag
     still runs. `tools/ci/check_integration_shards.sh` fails when the three
     sources disagree
   - The only job that runs them. `--config=ci` in the `test` job expands
     `--config=fast`, whose `--test_tag_filters=-nested-bazel` filters them out;
     unfiltered they would run three times per push
   - The targets carry `cpu:2` in place of `exclusive`, so Bazel bounds how many
     nested Bazel servers run at once by the machine's cores
   - Each nested Bazel gets its own output base under the test's `TEST_TMPDIR`,
     inside `<outer output base>/execroot/_main/_tmp`, which the outer Bazel
     clears in full on each `bazel test`. A killed run leaves nothing that
     outlives the next invocation, and two checkouts running one test cannot
     share a directory. `/mnt/rules_ts_it` holds only the repository, disk,
     bazelisk and pnpm caches; the job's `df -h /mnt /`, before and after,
     records whether the tens of GB of output bases changed volume
   - `/mnt/rules_ts_it` is a bare `mkdir -p` on a fresh runner, and the cache
     step below restores only the four cache subdirectories, never the per-test
     output bases, so every nested output base starts empty on every run. A
     retained output base saves a local developer a measured ~13.5s per test
     and saves CI nothing
   - The harness appends `common --repository_cache=<shared>` and
     `common --disk_cache=<shared>` to every staged workspace's `.bazelrc`
     (`prepare()` in `tests/integration/harness/harness.go`). Without the shared
     cache each workspace fetches the whole BCR registry for itself, and the
     resulting lookup failures read as flaky tests. A workspace that carries a
     lockfile is installed by the runner (`Install()`) with the workspace's
     `ts_pnpm`, its store under `/mnt/rules_ts_it/pnpm`, before Gazelle lists
     it
   - The harness's persistent root is `RULES_TS_IT_SCRATCH` when set (CI's
     `/mnt/rules_ts_it`), else `$XDG_CACHE_HOME/rules_typescript_it`, else
     `~/.cache/rules_typescript_it`, else `os.TempDir()`, last because `$TMPDIR`
     can be a tmpfs; never `TEST_TMPDIR`, which the outer Bazel clears on each
     `bazel test`, so a cache placed there would be re-fetched every run. It
     holds the repository, disk, bazelisk and pnpm caches. `BAZELISK_HOME`,
     unless inherited, points into it: bazelisk defaults it to `$PWD`, the
     per-run workspace, so left unset every test fetched Bazel from
     `releases.bazel.build` (~1.2GB a suite; a runner whose DNS timed out is
     what surfaced it, since Bazel echoes a test's stdout only when the test
     fails). A runner invoked by hand has no `TEST_TMPDIR`; its run root is then
     keyed by the checkout's hash and the test's name under that root rather
     than a fresh `os.MkdirTemp` name per process, so a killed run's multi-GB
     output base is overwritten by that test's next run instead of leaking
     under a name nothing finds again
   - `/mnt` is recreated every run, so an `actions/cache@v6` step restores
     `/mnt/rules_ts_it/repository_cache`, `/mnt/rules_ts_it/disk_cache`,
     `/mnt/rules_ts_it/bazelisk` and `/mnt/rules_ts_it/pnpm` under the key
     `nested-bazel-<runner.os>-<hash of MODULE.bazel, tests/npm/pnpm-lock.yaml,
     tests/integration/**/pnpm-lock.yaml, oxc_cli/Cargo.lock, .bazelversion>`,
     with `nested-bazel-<runner.os>-` as the restore-key prefix. One key serves
     both legs; only the first leg to finish saves it. Cold, the concurrent
     servers all miss the shared cache at once and fetch the same artifacts, a
     measured ~4GB (`tests/integration/tags.bzl`). The cache is
     content-addressed, so a stale restore is a miss, never a wrong answer.
     `.bazelversion` is in the key because the bazelisk directory holds one
     Bazel binary named by version, and `actions/cache` never overwrites an
     existing key
   - A step then runs `bazelisk --version` once with
     `BAZELISK_HOME=/mnt/rules_ts_it/bazelisk` and `USE_BAZEL_VERSION` read off
     `bazel_binaries.download(version = ...)` in `MODULE.bazel`, so on a cold
     cache one download primes what every nested Bazel would otherwise fetch at
     the same instant

6. **Linting & Code Quality** (`lint`)
   - `buildifier --mode=check -r .`, using the released `v8.2.1` binary
     downloaded in the job. There is no `buildifier` `bazel_dep`, so
     `bazel run @buildifier//:buildifier` fails with "No repository visible as
     '@buildifier'". See
     [CONTRIBUTING.md](contributing.md#starlark-build-files-and-bzl-files)
   - `gofmt -l .` (a non-empty result fails) and `go vet` over the Go modules,
     both through `actions/setup-go`: they read the `go.work` module graph off
     the source tree, so nothing about them comes from the build graph.
     `tools/quickstart` is named out of the `go vet` patterns because its
     `go:embed` target is a genrule output and `go list` failing there aborts the
     whole `./...` pattern
   - `mkdocs build --strict` into a temporary site directory. `docs.yml` builds
     the site only on push to `main`, so without this step a broken nav or page
     reference is caught only after merge

### Test Source Coverage

`bazel build //...`, `bazel test //...` and a byte-identical Gazelle rerun are
all satisfied by a Gazelle run that deletes a test target.
`check_test_sources.sh` is not: the set of test files on disk is not something
Gazelle writes.

Tagging a test `manual` defeats the same three checks: the target still exists,
still claims its srcs, and `//...` skips it. Each file's claim is checked twice:
against every test target, and against only the targets `bazel test //...`
runs. A file with the first claim but not the second is manual-only and has to
be named in the script's `MANUAL_ONLY` list with a reason. The list is exact in
both directions: tagging a test `manual` fails until the reason is written down,
and untagging it fails until the entry is removed.

Directories holding their own `MODULE.bazel`, and `.bazelignore` roots, are out
of scope: `//...` does not descend into them.

The script is read-only: a loading-phase `bazel query` and `git ls-files`, no
Gazelle run and no writes. `git ls-files` cannot see an unstaged new file, so a
local run reports green on a test that has no target yet.

### Retired Names

A retired attribute, kind, provider, directive, export or file path that a page
or a comment still names is a sentence the code falsified.
`check_retired_names.sh` carries the list and greps every tracked file for it in
identifier form: whole words (`WORDS`), the `ts_` directive prefix after the
`# gazelle:` marker, and the spellings a whole word misses (`PATTERNS`): a
deleted file's path, a target name Gazelle or the test macro wrote, a retired
output or target suffix. Out of scope:
`changelog.d/` and `CHANGELOG.md`, where a retirement is recorded with the edit
it requires; `TODO.md` and `rules-ts-v2-project-plan.md`, the project's
history; the rows of `docs/gazelle/directives.md`'s table mapping each retired
directive to its replacement. Two names are left off the list because they are
live under another meaning, `module_name` (bzlmod's `git_override` keyword) and
`jsx_import_source` (an oxc_cli option field); a name that is also a path
under `tests/` (`PATH_CLASHING`) matches only outside a path.

A file that asserts a retired name is absent or inert -- `kinds_surface_test.go`
pins the kinds Gazelle no longer writes -- is listed in `ALLOWED` inside the
script with the reason. The list is exact in both directions: a listed file
with no hit fails until the entry is removed. `git grep` only, no Bazel.

### Coverage Report

`bazel test //...` never makes a coverage run, and a coverage run whose report
is empty passes: Bazel's `collect_coverage.sh` merges what the test left under
`COVERAGE_DIR`, and an empty directory is an empty `coverage.dat`.
`check_coverage_report.sh` runs `//tests/vitest/coverage:math_coverage_test`
under `bazel coverage --combined_report=lcov` twice, with the default
`--instrumentation_filter` and with `^//tests/vitest[/:]`, and compares the
combined report's `SF:` lines with the files each filter selects
([ts_test § Coverage](rules/ts-test.md#coverage)). It is the last step of the
`test` job, after the suite, on ubuntu only.

### Triggering CI

Pushes to `main` and `develop`, and every pull request, with no branch filter:
a stacked PR targets the branch below it, and a filtered `pull_request` fires no
run for one. There is no `workflow_dispatch` trigger, so the Actions tab offers
no "Run workflow" button: re-run a failed job, or push.

## Running CI Locally

There is no CI driver script: the stages live in `.bazelrc` as `--config=`
groups, so the local command and the workflow step are the same command.

```bash
# The main workspace: tests, then type-checking. --config=ci expands
# --config=fast, whose --test_tag_filters=-nested-bazel drops the
# nested-Bazel targets.
bazel test --config=ci //...
bazel build --config=ci //... --output_groups=+_validation

# The nested-Bazel suite (~3 minutes per target, as many at once as the
# machine has cores for).
bazel test --config=ci-integration //tests/integration/...

# e2e/ and examples/ are separate workspaces (.bazelignore), so they are
# separate invocations — a --config cannot change workspace.
cd e2e/basic && bazel run //:pnpm -- install --frozen-lockfile && \
  bazel run //:gazelle -- -mode=diff && bazel build //... && bazel test //...
cd examples/basic && bazel run //:gazelle -- -mode=diff && bazel build //...
```

## Determinism Verification

Two builds cannot be a single Bazel action, so the check is a script of
invocations: `tools/ci/check_determinism.sh DIR` builds `//tests/smoke:hello`
and the four Go tools under `--config=determinism
--platforms=//platforms:linux_amd64` from the empty output bases `DIR/a` and
`DIR/b`, with local execution and no disk or remote cache, and compares
every file of that
configuration byte for byte. The `determinism` job runs it (see
[Workflow Jobs](#workflow-jobs) job 4); locally:

```bash
tools/ci/check_determinism.sh "$HOME/.cache/rules_ts_det"
```

The files are read out of the configuration the build used --
`cquery --output=files "config(set(<targets>), target)"` under the build's
flags, as `check_tools_lock.sh` reads the tools -- because `bazel info
bazel-bin` names the top-level output directory alone: the four `go_binary`
targets are `pure = "on"`, a rules_go transition, and their binaries are
written to a `-ST-<hash>` directory beside it. `--config=determinism` turns
off the convenience symlinks, which the two builds would otherwise race for.
Separate output bases stand in for `bazel clean` and preserve the repository
cache; the script refuses an output base that is not empty. The check has no
nested test of its own: its two builds are cold by design, and the job runs
it on every pull request update and push to main or develop.

## Known Sources of Non-Determinism

Eight places non-determinism can enter, the current status of each, and what a
rule of your own has to do.

### 1. Build Timestamps in Compiled Output

**Risk**: A compiler that embeds the current timestamp in its output.
**Status in rules_typescript**: neither `oxc` nor `tsgo` embeds a timestamp
in a compiled `.js` or `.js.map`, and `tsgo` embeds none in a `.d.ts`. The
`determinism` CI job compares the `.js` and `.js.map` of `//tests/smoke:hello`
across two builds.
**Mitigation**: A `genrule` running a tool that calls `date` is non-deterministic. Pass `--no-timestamp` or the equivalent to that tool.

### 2. File Ordering in Directory Outputs

**Risk**: With `ctx.actions.declare_directory`, file ordering inside the directory follows the filesystem's readdir order, which varies across kernels and filesystems.
**Status in rules_typescript**: The rules with a declared output directory (`npm_store`, `ts_codegen`) copy files by name: a store tree is a compile input, deterministic by construction (one `tsaction stage` over the fetched package), read as files and never as a listing, so ordering matters only in a byte-for-byte directory comparison.
**Mitigation**: Check directory artifacts with `diff -r`, which is order-insensitive; `tar c ... | sha256sum` is not.

### 3. Vite Bundle Content Hashes

**Risk**: Vite (and Rollup underneath it) names chunk files by content hash. The algorithm is deterministic, but chunk boundaries depend on module graph traversal order, which changes when `import()` statements are added or removed.
**Status**: Deterministic for a fixed source tree. A source change changes every dependent chunk hash, which is correct.
**Mitigation**: None needed. Do not compare Vite output hashes across source versions.

### 4. npm Package Download Order

**Risk**: parallel npm tarball downloads; if two packages produced the same
output file path, the winner would depend on download order.
**Status**: not possible. Each package is its own external repository with its
own root, so there is no shared output path to race over and no cross-package
ordering dependency.
**Mitigation**: N/A.

### 5. tsgo (TypeScript Native) Internal Parallelism

**Risk**: `tsgo` type-checks with goroutines, so diagnostic message ordering can vary between runs on different hardware.
**Status**: tsgo `.d.ts` outputs are deterministic (Go's `sort.Slice` is not random). Diagnostic ordering is consistent within one binary and may differ between tsgo versions.
**Mitigation**: Pin the tsgo version. This repository pins it through `ts/private/tsgo/pnpm-lock.yaml`, the lockfile the `ts` extension reads when no `ts.tsgo()` is called; a consumer points the extension at its own with `ts.tsgo(pnpm_lock = "//:pnpm-lock.yaml")`, so the toolchain moves only when `pnpm install` moves `typescript`.

### 6. Environment Variable Leaks

**Risk**: An action reading an env var it does not declare in its `env` map takes the host's value, which differs between machines.
**Status**: Bazel's sandbox blocks undeclared env vars for rules with `use_default_shell_env = False`. Every rule in rules_typescript uses the sandbox with no default shell env.
**Mitigation**: `--incompatible_strict_action_env`, set in this repository's `.bazelrc`, replaces the inherited client environment with a fixed one, so an action that reads a var it never declared reads the same value on every machine.

### 7. Host Interpreters and Utilities

**Risk**: An action shelling out to a host interpreter or coreutil produces
whatever that version produces.
**Status**: not applicable. There is no Python in the ruleset; the house rule is
Starlark's `json.decode`/`json.encode` or awk. No build action runs a host
interpreter: the store copier and the launcher are Go.
**Mitigation**: none needed. In a `genrule` of your own, reach for a toolchain
input.

### 8. Gazelle-Generated BUILD Files

**Risk**: Gazelle updates BUILD files in place. Two developers on different OS/filesystem configurations (e.g. different file listing order) can generate different files.
**Status**: The Gazelle TypeScript extension sorts every generated `srcs`, `deps` and other list attribute. Generated BUILD files are deterministic for a fixed source tree.
**Mitigation**: Add a CI step `bazel run //:gazelle && git diff --exit-code`, so the checked-in BUILD files match what Gazelle generates.

### Summary Table

| Source | Affects | Deterministic? | Notes |
|--------|---------|---------------|-------|
| compiled .js/.js.map, oxc's or tsgo's | Compilation | Yes | No timestamps |
| tsgo generated .d.ts | Type checking | Yes | Sorted output |
| Vite bundle | Bundling | Yes (per source tree) | Chunk hashes change with source |
| store tree (`npm_store`) | Compile inputs, runtime | Yes | one copy per resolution, restored as files from a cache |
| Gazelle BUILD generation | Repo structure | Yes | sorted output |

## Guarantees

- **Determinism** is verified by the `determinism` CI job over the targets it
  names, and is not a blanket property of every rule.
- **A release tarball** is `git archive` over a tag, so it is a function of the
  commit.
- **A tools release asset** is the four Go tools built at the `tools-v<N>` tag
  with `bazel build --platforms=//platforms:<key>`, each `pure = "on"` (no
  cgo, so no builder's C library in the bytes), packed by `//tools/toolpack`,
  so it is a function of the commit; `tools/ci/check_tools_lock.sh` rebuilds
  the four and compares their SRIs with `ts/private/tools_lock.bzl` during optional release preparation and on the tag.
- **Sandbox isolation** is the sandbox's, with no default shell env; see
  [Environment Variable Leaks](#6-environment-variable-leaks).

## Release Process

### Prerequisites

- Clean working tree
- A valid semantic version (X.Y.Z or X.Y.Z-prerelease)

### Cutting a Release

```bash
bazel run //tools/release -- 0.2.0 --dry-run   # prints every step, writes nothing
bazel run //tools/release -- 0.2.0 --push
```

The tool validates the version, stops on a dirty tree or an existing tag,
rewrites the version inside `module()` in `MODULE.bazel` (and nowhere else, so
`bazel_dep` versions survive), commits, tags, and optionally pushes. It works on
the checkout you ran `bazel` from, via `BUILD_WORKING_DIRECTORY`.

Everything after the tag is `.github/workflows/release.yml`: `git archive`
tarball, SRI hash, GitHub release with a provenance attestation, and the PR that
fills in `.bcr/source.json`. The tool builds no tarball, because a locally built
one would differ from the published one and carry the wrong integrity hash.

Full walkthrough: [Release Process](RELEASE_PROCESS.md).

### Cutting a Tools Release

```bash
bazel run //tools/release -- tools 1 --push
```

Tags `tools-v<N>` on `HEAD`, the `N` `ts/private/tools_lock.bzl` names; the
`tools` job of `release.yml` builds the four assets, asserts their SRIs are the
table's, attaches them to the `tools-v<N>` release and attests them. Consumers that explicitly enable prebuilt tools download those assets.
[Release Process § Tools](RELEASE_PROCESS.md#tools) has the PR flow.

## Tools

Normal CI builds the four Go tools from the current source tree. The determinism job builds them from two empty output bases and compares their bytes. Toolpack unit tests check archive contents and reproducibility. Normal CI does not require a tools release tag, a release download or equality with the optional release lock table.

The optional prebuilt configuration and its publication steps are described in [Release Process](RELEASE_PROCESS.md#tools). Release workflows check the selected release table when an owner publishes a tools tag.

## Rust dependency resolution

rules_rs resolves the checked-in Cargo.toml and Cargo.lock pairs during Bazel module evaluation. Native unit tests build those resolved crates directly; there is no separate vendored-crate rendering job.

## BCR (Bazel Central Registry) Publishing

A BCR submission carries three files: `.bcr/metadata.json` (module-level, one
file for every version), `.bcr/source.json` (the tarball URL, its SRI hash and
`strip_prefix`) and `.bcr/presubmit.yml`. The `update-bcr` job in
`.github/workflows/release.yml` rewrites `source.json` and opens a PR with it;
`metadata.json` and `presubmit.yml` are hand-maintained, and the job only prints
them back for the log. The field-by-field walkthrough, the `presubmit.yml`
matrix and the submission steps are in [BCR Submission](BCR_SUBMISSION.md).

Two workflows touch the `.bcr` files, and they split the work. `update-bcr`
computes and writes: it reads the SRI hash off the `release` job it depends on,
rewrites `source.json`, and opens the PR. `publish-bcr`, the only job in
`.github/workflows/publish-to-bcr.yml`, writes nothing to the repository. It
checks that all three `.bcr` files exist and that the two JSON ones parse
(`jq -e`), `HEAD`s the tarball URL and confirms the GitHub release exists (both
print a warning and carry on, neither fails the job), prints the manual
submission checklist, and uploads the three files as a 30-day artifact. Neither
job opens the pull request against the registry; that is done by hand.

Only `release.yml` runs from a tag push: a `v*` tag for a module release, a
`tools-v*` tag for a tools release. `publish-to-bcr.yml` triggers on
`workflow_dispatch` and on `release: [published]`, and the release that
`release.yml` creates does not fire it: GitHub starts no workflow run from an
event created with the default `GITHUB_TOKEN`, which is what
`softprops/action-gh-release` uses here. A release published by hand does fire
it.

Run it by hand after the `source.json` PR merges:

```bash
gh workflow run publish-to-bcr.yml -f version=0.2.0 -R mikn/rules_typescript
```

It reads `.bcr/source.json` off the checked-out branch. Run any earlier than
that and it validates and uploads the previous version's URL and hash. On a
release event it also runs `gh release edit --notes`, which replaces the release
notes with one line pointing at the metadata; the step ends in `|| true`, so it
never fails the job.

`<VERSION>` is the tag with its leading `v` stripped, which the workflow does
once (`VERSION="${TAG#v}"`) before building all three strings. The `v` therefore
appears in the release path and nowhere else: the tarball is
`rules_typescript-0.2.0.tar.gz` under tag `v0.2.0`, and `strip_prefix` matches
the `git archive --prefix` that produced it. That is the first thing to check
against a mismatched hash.

## Remote Caching

!!! note "Documented, not exercised"
    Nothing in this repository's own CI uses `--remote_cache` or RBE. The setups
    below are configurations we believe are right but do not run, and no
    cache-hit figure on this page was measured here. The disk cache is
    exercised: `//tests/integration:store_cache_test` rebuilds a store tree
    over a populated disk cache in a fresh output base and runs a test in it
    under `--nobuild_runfile_links` and under `--remote_download_outputs=minimal`.
    Remote execution is not.

A remote cache lets one machine reuse another's action outputs. Determinism is
what makes that safe; see
[Determinism Verification](#determinism-verification) for what is checked.

### BuildBuddy Setup

[BuildBuddy](https://www.buildbuddy.io) is a hosted remote cache with a free
tier. Create an account at https://app.buildbuddy.io for an API key, then add to
your workspace `.bazelrc`:

```
# Remote cache via BuildBuddy.
build:bb --remote_cache=grpcs://remote.buildbuddy.io
build:bb --remote_header=x-buildbuddy-api-key=<YOUR_API_KEY>
# Optional: upload local results so CI hits also benefit teammates.
build:bb --remote_upload_local_results
# Optional: stream build events to the BuildBuddy UI.
build:bb --bes_backend=grpcs://remote.buildbuddy.io
build:bb --bes_results_url=https://app.buildbuddy.io/invocation/
```

```bash
bazel build //... --config=bb
```

For CI, add `--config=bb` to every `bazel build` / `bazel test` invocation.

### EngFlow Setup

[EngFlow](https://www.engflow.com) is a commercial Bazel cache and RBE provider used by larger teams.

```
# .bazelrc
build:engflow --remote_cache=grpcs://your-cluster.engflow.com
build:engflow --remote_header=Authorization=Bearer <TOKEN>
build:engflow --remote_upload_local_results
```

### Self-Hosted Bazel Cache

For air-gapped or cost-sensitive environments you can run a minimal HTTP cache:

```bash
# Using bazel-remote (open source)
docker run -u 1000:1000 -v /path/to/cache:/data \
  -p 9090:9090 buchgr/bazel-remote-cache \
  --max_size 10
```

Then in `.bazelrc`:

```
build:local-cache --remote_cache=http://localhost:9090
```

### Verifying Hermeticity

All actions run inside the Bazel sandbox. To take the network away from them and confirm there are no hidden external dependencies:

```bash
bazel build //... --sandbox_default_allow_network=false
```

A clean build succeeds with no network errors. An action that fails here is downloading something, and the rule needs to declare that dependency explicitly.

Common sources of non-hermeticity:
- Shell scripts that call `curl` or `wget` without declaring network access.
- Node scripts that call `npm install` at build time.
- Toolchain binaries that phone home on first run (common with some TypeScript tools).

### Cache Hit Rate Tuning

1. **`--remote_upload_local_results`**: local developer builds populate the shared cache.
2. **Keep `--workspace_status_command` outputs stable**: stamp variables embedded in binaries bust the cache for every commit. Do not stamp library targets.
3. **Check for volatile env leaks**: `bazel build //... --action_env` shows every env var that actions see; only variables that affect outputs should be present.

---

## Remote Execution

Remote execution (RBE) runs actions on a pool of workers. Same caveat as remote
caching: nothing here is exercised by this repository's CI.

### Prerequisites

1. A compatible RBE backend (BuildBuddy RBE, EngFlow, Google RBE, or self-hosted).
2. A Docker image containing the build toolchain (oxc-bazel, Node.js, tsgo).
3. Platform constraints declared in your workspace (see below).

### Platform Constraints

Bazel selects toolchain binaries by execution platform, so RBE needs one declared. Add a `platforms` target to your workspace:

```python
# platforms/BUILD.bazel
platform(
    name = "linux_x86_64",
    constraint_values = [
        "@platforms//os:linux",
        "@platforms//cpu:x86_64",
    ],
)
```

And reference it in `.bazelrc`:

```
build:rbe --host_platform=//platforms:linux_x86_64
build:rbe --platforms=//platforms:linux_x86_64
```

### Toolchain Binary Compatibility

The toolchain binaries an executor runs:

| Tool | Source | Platforms |
|------|--------|-----------|
| `oxc-bazel` | Built from Rust source via rules_rs | whichever exec platform the build runs on |
| `tsgo` | Downloaded npm package; under `//ts/toolchain/tsgo_source`, built from Go source via rules_go | linux-x64, linux-arm64, darwin-x64, darwin-arm64; whichever exec platform the build runs on |
| `tsaction`, `lcov_merger`, `copy_to_workspace` | Built from source with rules_go; static Go, exec platform | linux-x64, linux-arm64, darwin-x64, darwin-arm64 |
| `ts_launcher` | Built from source with rules_go; static Go, target platform | linux-x64, linux-arm64, darwin-x64, darwin-arm64 |
| Node.js | JS runtime toolchain | linux and macOS on x86_64/arm64, Windows on x86_64 |

`oxc-bazel`, and `tsgo` under `//ts/toolchain/tsgo_source`, are compiled on the executor itself, so they match whatever the worker runs. The Go action helpers are built from source for the execution platform. The launcher is built from source for the target platform. The lockfile’s `tsgo` and Node.js use downloaded binaries.

Native binaries and node:test runners use a build-owned runtime view and exec the configured runtime. They need no process-exit watcher or cleanup helper. Use `tsaction` and `ts_launcher` from the same ruleset revision; older prebuilt tools do not understand the native-view action and config. See [runtime input lifetime](rules/providers.md#runtime-input-lifetime).

### BuildBuddy RBE Setup

BuildBuddy offers managed RBE with a free tier. To enable:

```
# .bazelrc
build:rbe --config=bb
# RBE-specific overrides.
build:rbe --remote_executor=grpcs://remote.buildbuddy.io
build:rbe --jobs=100
build:rbe --remote_instance_name=rules_typescript
```

The one host utility an executor needs is `bash`, which the BuildBuddy image
has. Everything else an action runs (node, tsgo, oxc, pnpm) is a toolchain
input.

### EngFlow RBE Setup

```
# .bazelrc
build:rbe --remote_executor=grpcs://your-cluster.engflow.com
build:rbe --jobs=200
build:rbe --remote_instance_name=default
```

### Custom Executor Image

For additional system tools, build on the minimal image:

```dockerfile
FROM ubuntu:22.04
# Only a POSIX shell is needed: the node_modules fallback taken when no JS
# runtime toolchain is registered is a bash action.
# Everything else runs a declared binary — no host tar, no python, no coreutils
# dependency.
RUN apt-get update && apt-get install -y \
    bash \
    && rm -rf /var/lib/apt/lists/*
```

Push to a container registry and configure in EngFlow or your self-hosted RBE cluster.

### Testing RBE Locally

To test RBE connectivity without running the whole build:

```bash
bazel build //tests/smoke:hello --config=rbe --verbose_failures
```

A successful build confirms the RBE worker receives actions and the toolchain binaries are executable on the remote platform.

---

## GitLab CI Template

Add this as `.gitlab-ci.yml`, or import it from a shared template repository:

```yaml
# GitLab CI/CD template for rules_typescript workspaces.
# Adjust the image, cache backend, and registry variables to match your setup.

variables:
  # The Bazel remote cache address. Leave empty to disable remote caching.
  BAZEL_REMOTE_CACHE: ""
  # BuildBuddy API key (or your remote cache auth header).
  BUILDBUDDY_API_KEY: ""

default:
  image: ubuntu:22.04
  before_script:
    - apt-get update -qq && apt-get install -y -qq
        curl git tar unzip
    # Install Bazel using Bazelisk.
    - curl -fsSL https://github.com/bazelbuild/bazelisk/releases/latest/download/bazelisk-linux-amd64 -o /usr/local/bin/bazel
    - chmod +x /usr/local/bin/bazel
    # Warm up the Bazel server and download toolchains once.
    - bazel version

stages:
  - test
  - build

# ── Unit tests ─────────────────────────────────────────────────────────────────
unit-tests:
  stage: test
  script:
    - |
      CACHE_FLAGS=""
      if [[ -n "$BAZEL_REMOTE_CACHE" ]]; then
        CACHE_FLAGS="--remote_cache=$BAZEL_REMOTE_CACHE"
        if [[ -n "$BUILDBUDDY_API_KEY" ]]; then
          CACHE_FLAGS="$CACHE_FLAGS --remote_header=x-buildbuddy-api-key=$BUILDBUDDY_API_KEY"
        fi
        CACHE_FLAGS="$CACHE_FLAGS --remote_upload_local_results"
      fi
      bazel test //... $CACHE_FLAGS --cache_test_results=no
  artifacts:
    reports:
      junit: bazel-testlogs/**/test.xml
    when: always
    expire_in: 7 days
  cache:
    key: bazel-$CI_COMMIT_REF_SLUG
    paths:
      - .cache/bazel/

# ── Build examples ─────────────────────────────────────────────────────────────
build-examples:
  stage: build
  script:
    - |
      CACHE_FLAGS=""
      if [[ -n "$BAZEL_REMOTE_CACHE" ]]; then
        CACHE_FLAGS="--remote_cache=$BAZEL_REMOTE_CACHE"
        if [[ -n "$BUILDBUDDY_API_KEY" ]]; then
          CACHE_FLAGS="$CACHE_FLAGS --remote_header=x-buildbuddy-api-key=$BUILDBUDDY_API_KEY"
        fi
      fi
      bazel build //examples/... $CACHE_FLAGS || true  # non-critical
  allow_failure: true

# ── Determinism check ──────────────────────────────────────────────────────────
determinism:
  stage: build
  script:
    - tools/ci/check_determinism.sh "$CI_PROJECT_DIR/.det"
  allow_failure: false
```

For GitLab's `cache:` key to cover the local Bazel cache, point the output base into it:

```
# .bazelrc  (add to the repo-level file)
startup --output_base=/root/.cache/bazel/output
```

---

## Troubleshooting

### Determinism Failures

Read the differing byte offset `cmp` names first: a difference early in a `.js`
is usually a path that leaked in, one late is usually a timestamp. Then work
through [Known Sources of Non-Determinism](#known-sources-of-non-determinism).
The two that reach a plain `ts_compile` target are a `genrule` of your own
calling a host tool, and an undeclared env var.

### Release Tool Issues

- **Dirty working tree**: commit or stash all changes; `--dry-run` reports what
  is uncommitted without touching anything
- **Tag exists**: `git tag -d <tag>` before push, or release the next patch
  version
- **"no rules_typescript MODULE.bazel found"**: `bazel run` was invoked from
  outside the checkout; the tool resolves the repo from
  `BUILD_WORKING_DIRECTORY` upward

### CI Failures

Open the failed job's log in GitHub Actions, then reproduce locally with
`bazel test --config=ci //...`.

## Related Documentation

- [Documentation index](index.md)
- [Release Process](RELEASE_PROCESS.md) — the walkthrough this page summarises
- [AGENTS.md](https://github.com/mikn/rules_typescript/blob/main/AGENTS.md) — architecture, for contributors
- [TODO.md](https://github.com/mikn/rules_typescript/blob/main/TODO.md) — roadmap

# Quick Start

The only prerequisite is **Bazelisk** (or Bazel 9+ directly). Bazel fetches
everything else hermetically on the first build: the Rust toolchain, the
Node.js runtime, tsgo, the Go tools of the [tools release](../RELEASE_PROCESS.md#tools),
and the npm packages your targets reach; a Go SDK when `bazel run //:gazelle`
compiles Gazelle and when the build registers
[tsgo from source](../rules/providers.md#tsgo-from-source). It also compiles
`oxc-bazel` from Rust source, which dominates the wall time: expect minutes at
any project size. After that everything is cached.

Choose your path:

- [Depending on rules_typescript](#depending-on-rules_typescript) — pinning the ruleset before it reaches the Bazel Central Registry
- [Path A: New project](#path-a-new-project) — starting from scratch
- [Path B: Existing project](#path-b-existing-project) — migrating a TypeScript codebase

---

## Install Bazelisk

Bazelisk reads `.bazelversion` and downloads the correct Bazel version automatically.

```bash
# macOS (Homebrew)
brew install bazelisk

# Linux / macOS (manual)
curl -Lo ~/.local/bin/bazel \
  https://github.com/bazelbuild/bazelisk/releases/latest/download/bazelisk-linux-amd64
chmod +x ~/.local/bin/bazel

# Windows (Scoop)
scoop install bazelisk
```

---

## Depending on rules_typescript

`rules_typescript` has no Bazel Central Registry entry and no module release
yet, so a bare
`bazel_dep(name = "rules_typescript", version = "0.2.0")` resolves against
nothing. Pin it with a non-registry override. All three forms
below keep the `bazel_dep` line, which is what makes the module a direct
dependency. bzlmod ignores the `version` value while a non-registry override is
active, and accepts the line with no `version` at all.

### git_override

```python
bazel_dep(name = "rules_typescript", version = "0.2.0")
git_override(
    module_name = "rules_typescript",
    remote = "https://github.com/mikn/rules_typescript.git",
    commit = "REPLACE_WITH_A_COMMIT_SHA_FROM_MAIN",
)
```

Use a full 40-character commit SHA. `git_override` re-resolves a branch name
whenever the repository cache is cold, which makes the build non-reproducible.

### archive_override

`git_override` runs a full `git clone` of the whole history, which carries
about 145 MB of packed cargo build output (524 MB uncompressed) tracked by
mistake before it was removed. A codeload tarball is one snapshot of the commit
and carries none of that history; prefer it on CI. Compute the integrity hash
for the commit you want:

```bash
COMMIT=<full 40-char sha>
curl -sL "https://github.com/mikn/rules_typescript/archive/$COMMIT.tar.gz" \
  | openssl dgst -sha256 -binary | openssl base64 -A
```

```python
bazel_dep(name = "rules_typescript", version = "0.2.0")
archive_override(
    module_name = "rules_typescript",
    urls = ["https://github.com/mikn/rules_typescript/archive/<sha>.tar.gz"],
    strip_prefix = "rules_typescript-<sha>",
    integrity = "sha256-<base64 output of the command above>",
)
```

### local_path_override

For a checkout on disk:

```python
bazel_dep(name = "rules_typescript", version = "0.2.0")
local_path_override(
    module_name = "rules_typescript",
    path = "../rules_typescript",
)
```

Once a version is published to the BCR, drop the override; the plain
`bazel_dep` line resolves on its own.

---

## Path A: New Project

**Step 1.** Create `.bazelversion`:

```
9.2.0
```

**Step 2.** Create `MODULE.bazel`, pinning `rules_typescript` with
`git_override` (see
[Depending on rules_typescript](#depending-on-rules_typescript) for the
`archive_override` alternative):

```python
module(
    name = "my_project",
    version = "0.0.0",
)

bazel_dep(name = "rules_typescript", version = "0.2.0")
git_override(
    module_name = "rules_typescript",
    remote = "https://github.com/mikn/rules_typescript.git",
    commit = "REPLACE_WITH_A_COMMIT_SHA_FROM_MAIN",
)

register_toolchains("@rules_typescript//ts/toolchain:all")

bazel_dep(name = "gazelle", version = "0.51.3")
```

**Step 3.** Create `.bazelrc`:

```
build --incompatible_strict_action_env
build --nolegacy_external_runfiles
build --output_groups=+_validation
```

The `--output_groups=+_validation` line makes type errors fail `bazel build`, the same as `go build`.

`rules_typescript` uses rules_rs, so `@rules_rust` is not visible from your repository: a
`@rules_rust//...` flag here is rejected outright (see
[Troubleshooting](../guides/troubleshooting.md#no-repository-visible-as-rules_rust)).

**Step 4.** Create `BUILD.bazel` at the repo root:

```python
load("@gazelle//:def.bzl", "gazelle")

gazelle(
    name = "gazelle",
    gazelle = "@rules_typescript//gazelle:gazelle_typescript",
    tags = ["manual"],
)
```

**Step 5.** Write your TypeScript files, with a `tsconfig.json` in each
directory that is to be a package: Gazelle writes one `ts_compile` per
`tsconfig.json`, over what the program lists. Explicit return types are
optional; tsgo emits the declarations from the full type program:

```json
// src/lib/tsconfig.json
{ "compilerOptions": { "module": "preserve", "strict": true } }
```

```typescript
// src/lib/math.ts
export function add(a: number, b: number) {
  return a + b;
}
```

**Step 6.** Generate BUILD files:

```bash
bazel run //:gazelle
```

**Step 7.** Build and type-check:

```bash
bazel build //...
```

Each `ts_compile` target Gazelle generates writes `.js` and `.js.map` per
source file and runs `TsgoCheck`, the type check, as a validation of the
build: `bazel-bin/src/lib/math.js` and `math.js.map` for the file above.
`math.d.ts` is written when another package's compile imports `src/lib`, or
for every target under `bazel build //... --output_groups=+declarations`.

**Step 8.** Run tests, once there is one. With no `*.test.ts` there is no test
target, and Bazel treats that as an error:

```
$ bazel test //...
INFO: Found 2 targets and 0 test targets...
ERROR: No test targets were found, yet testing was requested
```

The exit code is 4. vitest comes from your lockfile, so the first test needs
the npm setup:

```bash
pnpm init
pnpm add vitest --lockfile-only
```

Those two run a pnpm of your own; the first lockfile is the one file the
hermetic pnpm cannot write. `npm.translate_lock` reads `pnpm-lock.yaml` while
`MODULE.bazel` is evaluated, so `bazel run //:pnpm` exists only once the file
does. From then on it edits the lockfile
([Hermetic pnpm](../guides/npm.md#hermetic-pnpm)).

```python
# MODULE.bazel: add to what Step 2 wrote
npm = use_extension("@rules_typescript//npm:extensions.bzl", "npm")
npm.translate_lock(pnpm_lock = "//:pnpm-lock.yaml")
use_repo(npm, "npm", "pnpm")
```

`"pnpm"` is the hermetic pnpm the `ts_pnpm` and `ts_add_package` targets in
your root `BUILD.bazel` run; write them by hand, and the checkout is installed
with `bazel run //:pnpm -- install` before Gazelle lists it, since tsgo resolves
a bare specifier through `node_modules/`. See
[npm Dependencies](../guides/npm.md#setup).

Write the test beside the source, re-run Gazelle, and test:

```typescript
// src/lib/math.test.ts
import { expect, it } from "vitest";

import { add } from "./math";

it("adds", () => {
  expect(add(2, 3)).toBe(5);
});
```

```bash
bazel run //:gazelle    # writes ts_test(name = "lib_test", ...)
bazel test //...        # //src/lib:lib_test  PASSED
```

See [Testing with vitest](../guides/testing.md) for DOM environments,
coverage, snapshots and sharding.

---

## Path B: Existing Project

**Step 1.** Set up the same root files as Path A: `.bazelversion`,
`MODULE.bazel` and `.bazelrc`.

**Step 2.** Create `BUILD.bazel` at the repo root:

```python
load("@gazelle//:def.bzl", "gazelle")

gazelle(
    name = "gazelle",
    gazelle = "@rules_typescript//gazelle:gazelle_typescript",
    tags = ["manual"],
)
```

Explicit return types stay optional: the `ts_compile` default emits
declarations with tsgo, which infers them.

**Step 3.** Wire up your `pnpm-lock.yaml` before the first build. Gazelle
resolves every bare import to an `@npm//…` label, so analysis fails until the
hub exists:

```python
# MODULE.bazel
npm = use_extension("@rules_typescript//npm:extensions.bzl", "npm")
npm.translate_lock(pnpm_lock = "//:pnpm-lock.yaml")
use_repo(npm, "npm", "pnpm")
```

`@npm` is the alias hub your `deps` labels spell; `@pnpm` backs the `ts_pnpm`
and `ts_add_package` targets you write into your root `BUILD.bazel`
([Hermetic pnpm](../guides/npm.md#hermetic-pnpm)). See
[npm Dependencies](../guides/npm.md) for private registries and patched
dependencies.

The lockfile is the build's one npm input. Gazelle's listing is tsgo over the
checkout, which resolves a bare specifier through `node_modules/`, so a
checkout runs `pnpm install` once; a root lockfile with no install is refused
before the first listing.

Bazel walks that `node_modules/`. pnpm links a `workspace:` dependency into it
(`workers/asset-viewer/node_modules/@example/design-system -> ../../../../packages/design-system`),
and `bazel build //...` can discover linked packages under `node_modules/`. Keep installed dependency trees out of recursive package discovery. `.bazelignore` takes no
globs; `REPO.bazel` does:

```python
# REPO.bazel
ignore_directories(["**/node_modules"])
```

**Step 4.** Run Gazelle:

```bash
bazel run //:gazelle
```

**Step 5.** Build everything:

```bash
bazel build //...
```

Type errors fail the build: `TsgoCheck` is a validation Bazel runs with the
build. "Missing return type" errors apply under `--//ts:declarations=oxc`,
and under a `tsconfig.json` that sets `isolatedDeclarations`.

A `baseUrl` in your `tsconfig.json` fails here. Gazelle wires the file onto
every target as `tsconfig = "//:tsconfig"`, and tsgo rejects the key wherever
it sits in the `extends` chain:

```
error TS5102: Option 'baseUrl' has been removed. Please remove it from your configuration.
```

Delete it. `paths` here is Bazel's and resolves against no `baseUrl`; see
[Option 'baseUrl' has been removed](../guides/troubleshooting.md#option-baseurl-has-been-removed).

!!! note "A `compilerOptions.paths` alias that crosses a target boundary"
    Your `paths` stay in your `tsconfig.json`, and both readers take them from
    there. The rule rewrites each value to its source and `bazel-bin` twins, so
    an aliased import reaches a dep's declarations where the build left them.
    tsgo resolves the alias when it lists the program, and Gazelle maps the
    file it landed on to the package that owns it and writes that package into
    `deps`; no attribute repeats the alias. `"@lib/*": ["../lib/*"]` in
    `src/app/tsconfig.json`:

    ```python
    # src/app/BUILD.bazel
    ts_compile(
        name = "app",
        srcs = ["main.ts"],   # import { add } from "@lib/math";
        tsconfig = ":tsconfig",
        visibility = ["//visibility:public"],
        deps = ["//src/lib"],
    )
    ```

    A bare specifier naming another package of the workspace is not an
    alias; see
    [importing another target by bare specifier](../rules/ts-compile.md#importing-another-target-by-bare-specifier).

    Gazelle generates a default oj dev target for application entries and
    updates its managed attributes. Every other attribute, such as an explicit
    `server` or `port`, and every `# keep` value remain. See [Dev Server](../guides/dev-server.md).

**Step 6.** Optional. Once a package's exports are all annotated, move it to
Oxc's syntactic declaration emit, which takes type-checking off the critical
path. See [Isolated Declarations](isolated-declarations.md).

---

## Scaffolding From a Checkout

A checkout of this repository writes the Path A files itself:

```bash
bazel run //tools/quickstart -- --dir ../my_project --rules-path "$PWD"
```

It writes ten files: `.bazelversion` (`9.2.0`), `.bazelrc`, `MODULE.bazel`, a
root `BUILD.bazel` holding the Gazelle target, `src/BUILD.bazel`,
`src/lib/math.ts`, `src/lib/index.ts`, `src/lib/BUILD.bazel`, `src/app/main.ts`
and `src/app/BUILD.bazel`. `src/lib` and `src/app` each hold a hand-written
`ts_compile`, `//src/lib` and `//src/app`, and no `tsconfig.json`; the
`math.ts` carries explicit return types. Neither directory is a package, so
`bazel run //:gazelle` withdraws both rules unless `# keep` sits above them;
a `tsconfig.json` in each makes them Gazelle's.

`--rules-path` adds a `local_path_override` naming the checkout; it has to be
absolute, because `bazel run` starts the tool inside its runfiles tree, not in
the checkout. Without it the
written `bazel_dep(name = "rules_typescript", version = "0.2.0")` resolves
against nothing, the failure at the top of this page, so pass it until the
ruleset is on the BCR. The `.bazelrc` it writes holds two lines,
`build --output_groups=+_validation` and `test --test_output=errors`, not the
three at Step 3. `--dry-run` lists the files without writing, `--force`
overwrites files that exist, and `--bazel-version` changes `.bazelversion`.

## Version Pinning

### TypeScript

The tsgo toolchain is a TypeScript 7 release. The `typescript` npm package is a
launcher whose Go compiler lives in per-platform optional dependencies
(`@typescript/typescript-linux-x64` and the like), so a pnpm lockfile that pins
`typescript` already states, for every platform, the tarball and the integrity
of the compiler your `tsc` runs. Point the `ts` extension at that lockfile and
the toolchain is the same compiler. Add to `MODULE.bazel`:

```python
ts = use_extension("@rules_typescript//ts:extensions.bzl", "ts")
ts.tsgo(pnpm_lock = "//:pnpm-lock.yaml")
```

The root importer's `typescript` entry names the version, Bazel verifies each
download against the lockfile's integrity, and a `pnpm install` that moves the
version moves the toolchain. The lockfile needs TypeScript 7 or later pinned at
the workspace root (`pnpm add -D typescript@7`); a version before 7, an alias
under the name, or two versions with no root pin fails at extension evaluation
naming the lockfile and the fix. `package = "@typescript/native-preview"` reads
the nightly's entry instead, and `npmrc = "//:.npmrc"` names the registry the
tarballs come from and its credentials, as it does for `npm.translate_lock`.
Only the root module's
`ts.tsgo()` takes effect; with no call the toolchain is the release
rules_typescript's own `ts/private/tsgo/pnpm-lock.yaml` pins.

The alternative is a version literal, for a build no lockfile describes:

```python
ts.tsgo(version = "7.0.2")
```

Nothing verifies that download: the registry tarball is fetched as is, with a
warning naming the URL. Prefer `pnpm_lock` wherever there is a lockfile.

### Node.js

The tests run under the Node your `.nvmrc` names. Add to `MODULE.bazel`:

```python
bazel_dep(name = "rules_nodejs", version = "6.7.5")

node = use_extension("@rules_nodejs//nodejs:extensions.bzl", "node")
node.toolchain(
    name = "nodejs",
    node_version_from_nvmrc = "//:.nvmrc",
)
```

Every `ts_test`, and every build tool that is node, runs under that version;
an edit to the file moves the runtime with it. The file holds the bare
version, `24.18.0`. `node_version = "24.18.0"` in place of the
`node_version_from_nvmrc` line pins the same without a file. With no
`node.toolchain()` call the runtime is the `22.23.1` `rules_typescript`'s own
`MODULE.bazel` pins: a default, not a decision for your workspace.
`//tests/integration/node_version` pins the three cases.

The `bazel_dep` line is required. `rules_nodejs` reaches your build as a
transitive dependency of `rules_typescript`, so without it `@rules_nodejs` is
not in your repository mapping and the `use_extension` fails with
`no repo visible as '@rules_nodejs' here`. `6.7.5` is the version
`rules_typescript`'s `MODULE.bazel` pins.

Keep `name = "nodejs"`. `rules_nodejs` keeps the root module's registration of
that name and ignores every other module's, so your version wins over the one
`rules_typescript` asks for. `rules_typescript`'s toolchains resolve the
repositories that name generates (`nodejs_linux_amd64` and friends). Under any
other name your registration is silently unused.

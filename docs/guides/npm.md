# npm Dependencies

npm packages come from a `pnpm-lock.yaml`. The `npm` module extension reads that
file, text only and no network, and declares one external repository per package
plus an alias hub named `@npm` that holds nothing but aliases. Bazel fetches a
package's repository the first time something needs it, so a target's npm cost is
its own dependency closure.

## Setup

**Step 1.** Create a `pnpm-lock.yaml`:

```bash
pnpm init
pnpm add react react-dom --lockfile-only
```

`--lockfile-only` updates the lockfile without creating a `node_modules/`
directory. Build actions use Bazel’s npm store: the build
declares pnpm's virtual store and each importer's links into it
([node_modules](../rules/node-modules.md)), and a target resolves through the
importer chain, in the sandbox and in a test's runfiles. Gazelle and the editor read checkout `node_modules`, so run `pnpm install` before generating BUILD files or using the editor
([IDE Setup](../getting-started/ide-setup.md#npm-packages)).

A `pnpm-lock.yaml` is the only npm input these rules read; there is no npm or
yarn lockfile path. A pnpm of your own writes the first one. Every edit after
that can go through the pnpm the extension downloads, `bazel run //:pnpm`: the
extension reads the lockfile when a command first reaches a repository it
declares, so with `npm.translate_lock` declared and no file at the label, a
target that needs no `@npm` package still builds, and `bazel run //:pnpm`
fails in the read. See [Hermetic pnpm](#hermetic-pnpm).

**Step 2.** Add to `MODULE.bazel`:

```python
npm = use_extension("@rules_typescript//npm:extensions.bzl", "npm")
npm.translate_lock(pnpm_lock = "//:pnpm-lock.yaml")
use_repo(npm, "npm", "pnpm")
```

`"npm"` is the alias hub your labels spell. `"pnpm"` is the hermetic pnpm the
`ts_pnpm` and `ts_add_package` targets in your root `BUILD.bazel` run; write
them by hand, and take the repo when you do, or `bazel build //...` stops
before it builds anything:

```
ERROR: no such package '@@[unknown repo 'pnpm' requested from @@ (did you mean
'npm'?)]//': The repository '@@[unknown repo 'pnpm' requested from @@ (did you
mean 'npm'?)]' could not be resolved: No repository visible as '@pnpm' from
main repository and referenced by '//:pnpm'
```

Gazelle lists each `tsconfig.json` with tsgo over the checkout, which resolves
a bare specifier through `node_modules/`, so the checkout is installed once
(`bazel run //:pnpm -- install`) before the first run. See
[Hermetic pnpm](#hermetic-pnpm).

**Step 3.** Reference packages in BUILD files:

```python
ts_compile(
    name = "app",
    srcs = ["app.ts"],
    deps = ["@npm//:zod", "@npm//:react"],
)
```

## Label Convention

| npm package | Bazel label |
|-------------|-------------|
| `react` | `@npm//:react` |
| `react-dom` | `@npm//:react-dom` |
| `@types/react` | `@npm//:types_react` |
| `@tanstack/react-query` | `@npm//:tanstack_react-query` |

- Scoped packages (`@scope/name`) become `scope_name`: drop the `@`, replace `/`
  with `_`.
- Hyphens are kept as-is.
- A bare label means the root importer's own resolution where the lockfile
  gives one, and the highest version otherwise. A package resolved at several
  versions also gets a version-suffixed label per version, so one can be pinned.
- A `workspace:*` link resolves to a target in your own repository. See
  [workspace links](#workspace-links).
- A target resolves a name to its importer's resolution: the link of the
  nearest lockfile importer at or above it that declares the name. A version
  another importer declares, or one only a dependency's closure carries, is
  not the target's to import, and naming it in `deps` fails analysis.

## Adding Dependencies

```bash
bazel run //:pnpm -- add zod   # updates pnpm-lock.yaml and installs it
bazel run //:gazelle           # tsgo lists the import; Gazelle adds @npm//:zod
bazel build //...              # Bazel fetches just that package's closure
```

`pnpm` edits the lockfile and installs the checkout Gazelle lists. It is not
needed at build time, test time, or on CI.

## Hermetic pnpm

The extension downloads a standalone pnpm binary whether or not one is asked
for, so lockfile edits need no system install. Write the two macros into the
root `BUILD.bazel`:

```python
# BUILD.bazel
load("@rules_typescript//ts:defs.bzl", "ts_add_package", "ts_pnpm")

ts_pnpm(name = "pnpm")

ts_add_package(
    name = "add_package",
    pnpm_lock = "//:pnpm-lock.yaml",
)
```

The `@pnpm` repo they need came from Step 2. `npm.pnpm()` pins which version is
downloaded; without it a default version is used:

```python
# MODULE.bazel
npm = use_extension("@rules_typescript//npm:extensions.bzl", "npm")
npm.pnpm(version = "10.34.9")   # optional; a default version is used otherwise
npm.translate_lock(pnpm_lock = "//:pnpm-lock.yaml")
use_repo(npm, "npm", "pnpm")
```

```bash
bazel run //:pnpm -- --version
bazel run //:pnpm -- add zod --lockfile-only
bazel run //:add_package -- zod          # appends --lockfile-only for you
```

`pnpm_lock` is the hub this target edits, spelled as its `npm.translate_lock()`
spells it; pnpm is pointed at that label's directory with `--dir`. It is
required: a `pnpm add` with no hub resolves against the workspace root and
writes a `package.json` and `pnpm-lock.yaml` there. Declaring a label also makes
a missing lockfile, or one this package cannot see, a build error.

A workspace with several hubs gets one target per hub, named after it:

```python
ts_add_package(
    name = "add_package_tailwind",
    pnpm_lock = "//third_party/tailwind:pnpm-lock.yaml",
)
```

Both targets `cd` to `$BUILD_WORKSPACE_DIRECTORY` first, so they edit the source
tree. The wrapper is a bash script, so it does not run on Windows.

Both macros take `pnpm_repo_name`, default `"pnpm"`: the repository holding the
binary, which `npm.pnpm(name = ...)` declares under the same default. Only the
root module's `npm.pnpm()` tag is read; a non-root module's is ignored. Every
workspace in this repository, the examples and the integration workspaces
included, uses the default name, and no test renames it.

### Which Lockfile the Wrapper Edits

Extra `pnpm add` flags are passed through. Three checks keep the rewrite inside
the hub, one per route pnpm has to that setting.

- **Flags.** Four spellings are rejected, because an appended
  `--lockfile-dir` would lose to an earlier `--lockfile-directory`:

  ```
  --lockfile-dir  --lockfile-directory  --config-lockfile-dir  --config-lockfile-directory
  ```

  Each argument is normalised first: case-folded, `.` and `_` turned into `-`,
  anything after an `=` stripped. `--LOCKFILE_DIR=x` and
  `--config.lockfile-directory=x` are refused too. `--dir` is allowed, because
  the one the target appends wins: pnpm takes the last occurrence.
- **Environment.** Every variable spelled `NPM_CONFIG_*` or `npm_config_*` is
  unset. The match is broad, so `NPM_CONFIG_REGISTRY` goes along with
  `NPM_CONFIG_LOCKFILE_DIR`; a mixed-case `Npm_Config_lockfile_dir` survives it.
- **Outcome.** The wrapper lists every `pnpm-lock.yaml` in the tree before and
  after the run. Any new one outside the hub is deleted and the target exits
  non-zero, which covers the mixed-case environment names and any other route to
  the same setting.

Two more refusals before pnpm runs: no `PNPM_HUB_DIR` (the target was not
generated by `ts_add_package`), and no `package.json` beside the hub's lockfile,
which pnpm would create.

To edit another hub, run that hub's own target.

## More Than One Hub

A workspace can translate several lockfiles. Each one is its own alias hub, named
by `npm.translate_lock`'s `name` attr, and that name is what `use_repo` takes and
what BUILD labels spell:

```python
# MODULE.bazel
npm = use_extension("@rules_typescript//npm:extensions.bzl", "npm")
npm.translate_lock(pnpm_lock = "//:pnpm-lock.yaml")                       # name defaults to "npm"
npm.translate_lock(name = "npm_tools", pnpm_lock = "//tools:pnpm-lock.yaml")
use_repo(npm, "npm", "npm_tools", "pnpm")
```

```python
deps = ["@npm_tools//:eslint"]
```

A second lockfile keeps a closure (eslint's, say) out of the tree an app's tests
resolve against, or keeps a curated fixture out of `pnpm add`'s reach. It costs
two lockfiles to keep in step, and a package resolved in both is fetched twice.
One root lockfile is the default
([Monorepo Layout](monorepo.md#single-pnpm-lockfile)).

Three things follow from a second hub:

- **Gazelle writes `@npm` alone.** Every npm label it writes names the hub of
  the root `pnpm-lock.yaml` it reads. A package whose imports come from another
  hub writes its `deps` by hand under `# keep`.
- **One `ts_add_package` target per hub.** pnpm rewrites whichever lockfile it
  resolves against, so the hub belongs in the command a person types:

  ```python
  ts_add_package(
      name = "add_package_tools",
      pnpm_lock = "//tools:pnpm-lock.yaml",
  )
  ```

  That hub is then edited with `bazel run //:add_package_tools -- eslint`.

- **A dev-only hub stays out of consumers' lock files.** A hub declared through
  `use_extension(..., dev_dependency = True)` is invisible when your module is
  not the root.

## Private and Scoped Registries

A pnpm lockfile records a package's `name@version` and its integrity, and no
registry anywhere. pnpm keeps the registry map in two files, and the extension
reads the same two:

- `pnpm-workspace.yaml` beside the lockfile: its `registries:` block (`default`
  and `@scope` keys) and a top-level `registry:`. Found by position, as pnpm
  finds it; nothing names it in `MODULE.bazel`.
- `.npmrc`, passed as `npmrc`: its `registry=` and `@scope:registry=` lines.

```yaml
# pnpm-workspace.yaml
registries:
  default: https://npm.example.com/artifactory/api/npm/npm-virtual/
  "@acme": https://npm.acme.test/
```

```
# .npmrc
registry=https://npm.example.com/          # the default for everything
@acme:registry=https://npm.example.com/    # the default for one scope
```

```python
npm.translate_lock(
    pnpm_lock = "//:pnpm-lock.yaml",
    npmrc = "//:.npmrc",
)
```

Use an indented `registries:` mapping with `default` or `@scope` keys, as above.
A `registries:` value written as a flow mapping (including `{}`) or YAML alias
is unsupported and fails with a diagnostic; expand it into the block form.
An absent or empty block is allowed.

The order is pnpm's (`@pnpm/config`, pnpm 11.19.0): the `.npmrc` lines, then
`registries:`, then `registry:`, the later setting winning a key. A value with a
`${VAR}` in `pnpm-workspace.yaml` is dropped, as pnpm drops it from a project's
file. A scoped package's tarball is
`<registry>/@scope/name/-/name-<version>.tgz` on its scope's registry, or the
default's. A `tarball:` in the lockfile's `resolution:` is an absolute URL pnpm
already resolved, and it wins over every setting.

### Credentials

Credentials are read at fetch time, never by the extension: the extension's
result is serialised into the committed `MODULE.bazel.lock`, so a token in an
attribute would be a token in git. Each package's own fetch reads two sources,
as pnpm does:

- `PNPM_CONFIG__AUTH` (or `pnpm_config__auth`) in the fetch environment, pnpm's
  `auth` setting: a JSON object keyed by registry URL, then by scope -- `"@"`
  for every package on that registry, `"@acme"` for one scope -- with
  `authToken` its one field. Reading it registers the variable, so a changed
  token refetches; no file holds it.

  ```bash
  export PNPM_CONFIG__AUTH='{
    "https://npm.acme.test": {"@acme": {"authToken": "..."}}
  }'
  ```

- the `.npmrc`'s `//host/path/:_authToken=` or `:_auth=` line, `${VAR}` expanded
  from the fetch environment. What lands in the lock is the file's label.

  ```
  //npm.example.com/:_authToken=${NPM_TOKEN}
  ```

For one URL the fetch sends the `auth` entry for the package's scope on the
longest registry prefixing the URL; without one, the unscoped credential on the
longest prefix, the `auth` setting's `"@"` entry over the `.npmrc`'s on one
registry. Credentials are keyed by `//host/path/`, so a registry mounted on a
path (an Artifactory repo, say) carries its own token without claiming the whole
host, and a token granted to one port stays off another.

pnpm 11 ignores a `${VAR}` credential in a project's `.npmrc` (the file is
committed, and the value could reach an attacker's registry), so a workspace it
installs keeps the token in `PNPM_CONFIG__AUTH`; the fetch reads the same
variable.

Three limits:

- **`~/.npmrc` is not consulted.** It lies outside the workspace, so Bazel
  cannot make it an input; the environment covers what varies per machine.
- **`username` / `_password` fail** with a message naming the file and the scope.
  npm stores `_password` base64-encoded and Starlark cannot decode it (there is
  no `chr()`). Use `_authToken`, which `npm config set //host/:_authToken`
  writes, or `_auth`, the same base64 blob.
- **A scope only the `auth` setting names is fetched from the default
  registry.** pnpm takes a scope's registry from the setting's key as well; the
  extension reads the two files alone, because the variable holds the token and
  an extension's inputs are recorded in the lock. Put the mapping in
  `pnpm-workspace.yaml`.

## Patched Dependencies

pnpm's `patchedDependencies` used to be ignored silently: the `packages:`
integrity in the lockfile is the upstream tarball's, so a patched package was
fetched unpatched.

Patches are passed as labels: the paths pnpm keeps in `pnpm-workspace.yaml`
cannot be turned into labels by an extension, because a path like
`patches/foo.patch` says nothing about where your Bazel package boundaries fall.

```python
npm.translate_lock(
    pnpm_lock = "//:pnpm-lock.yaml",
    patches = ["//patches:@acme__diffs@1.3.1.patch"],
)
```

Each file is matched to its lockfile entry by filename, pnpm's own convention
from `pnpm patch-commit`: `<name with / replaced by __>@<version>.patch`.

Every pairing is verified while the extension evaluates, so a patch nothing
currently depends on is checked too. Four failures, each naming the label:

- **the label resolves to no readable file.** Resolving the label also forces
  the patch's Bazel package to load, so a broken `patches/BUILD.bazel` surfaces
  here.
- **the file's sha256 disagrees with the digest `patchedDependencies` records.**
  pnpm writes that digest when it writes the patch, so a disagreement means the
  patch changed without `pnpm install` being re-run. A pre-pnpm-9 lockfile
  records something other than a sha256; the file still has to be readable, only
  the comparison is skipped.
- **a `patchedDependencies` entry with no matching label.**
- **a passed patch file no entry claims:** the lockfile is stale, or the file is
  misnamed.

!!! warning "A patch file whose name starts with `@`"
    `exports_files(glob(["*.patch"]))` cannot export it: `glob()` prefixes `:`
    onto such a result and `exports_files` rejects that as a target name, which
    fails the whole package and every patch in it. List those files literally:

    ```python
    exports_files(["@acme__diffs@1.3.1.patch", "nanoid@3.3.11.patch"])
    ```

## Integrity

Every `packages:` entry has to carry an integrity the download can be checked
against. A package whose `resolution:` has none used to be fetched with no
verification. That is now a hard error, raised while the extension evaluates, so
an entry nothing currently depends on is checked too:

```
npm: entries in //:pnpm-lock.yaml whose `resolution:` carries no usable integrity:
  unverified@1.0.0 -> resolution keys: tarball
Bazel would fetch these bytes with nothing to check them against, ...
```

Accepted algorithms are `sha512-`, `sha384-` and `sha256-`. The list is explicit,
so a pre-SRI digest (`sha1-`) is reported here, naming the package, before any
fetch turns it into a checksum error naming a URL.

Three lockfile shapes cannot satisfy it, all dependencies with no published
tarball: a git dependency (`{commit, repo, type: git}`), a
`file:` dependency on a local directory (`{directory, type: directory}`), and a
remote tarball pnpm could not hash (`{tarball}` with no `integrity`). Without
the check the first two fail later: with no `tarball:` key the registry URL is
built from the name and the fetch 404s. Depend on such a package as a
workspace member (a `link:` entry, which becomes a target in your own repository;
see [workspace links](#workspace-links)) or vendor its files.

There is no opt-out: the check runs at extension evaluation, and a module
extension cannot read build flags.

## `catalogs`, `overrides` and `packageExtensions`

pnpm resolves all three at every use site before writing the lockfile, so they
need no support and have none. `catalog:` specifiers, `overrides` (both plain and
the package-scoped `parent>child` form) and `packageExtensions` already appear as
concrete versions and injected peers in the `packages:` and `snapshots:` sections
the extension reads.

## Platform-Specific Packages

A package whose `os`/`cpu` fields exclude the platform
(`@rollup/rollup-linux-x64-gnu` on a Mac, say) is not part of the build; there
is nothing to configure.

A bin script that resolves an optional dependency at runtime (`oxlint` →
`@oxlint/binding-linux-x64-gnu`) gets it in its runfiles, though the two are not sibling
directories inside one repository.

## Where a Package's Type Declarations Come From

From the package's own `package.json`, read by tsgo where the package sits in
the store, `node_modules/.pnpm/<key>/node_modules/<name>/`, reached through the
importer's link. Nothing here reads `exports`, `types`,
`typings` or `main` for it. tsgo walks the tree as it walks a pnpm install --
the `exports` map in its own key order with the conditions as written, then
`typings` and `types`, then `main`, then the root index -- and a
`compilerOptions.types` entry or a `/// <reference types>` directive resolves
through the same tree by TypeScript's type-reference rules. So
`import type { TraceItem } from "@cloudflare/workers-types"` resolves to that
package's `index.ts`, a module, and `"types": ["@cloudflare/workers-types"]`
to its `index.d.ts`, a global script, as they do under `tsc`; an `exports`
subpath (`@cloudflare/vitest-pool-workers/types`) and a one-star pattern
(`"./*": "./dist/esm/*"`) resolve because tsgo reads the map itself.

A `.ts` module entry sits under `node_modules/<name>/` and is a library file to
TypeScript: type-checked, never emitted, outside the `rootDir` check. See
[the node_modules chain](../rules/ts-compile.md#the-node_modules-chain).

## What a Workspace Member Is Imported As

A `workspace:*` dependency resolves to a `link:` in the lockfile, and the hub
writes one `npm_workspace_package` view per workspace member -- every `link:`
target and every importer whose `package.json` has a `name`, one view per member
directory -- at `@npm//:<name>`; each importer that links the member holds a
`node_modules_member` target, `//<importer>:node_modules/<name>`, which a
target names in `deps`. An importer that declares the member's name with a
version instead of `workspace:` installs the published package of that name,
and a target under it names the importer-scoped label,
`@npm//packages/bundler-plugin:example-transform`, not the view. The view is that
member as an npm package: its store tree,
`node_modules/.pnpm/<name with / as +>@0.0.0/node_modules/<name>`, holds the
member's `package.json` as built beside the member's `.js`, `.js.map` and
`.d.ts` at the paths the manifest names. "As built" is one rewrite, done by the
member's `ts_compile` over the `package.json` in its `srcs` and written as
`<name>.package.json` beside the src, which stays staged as written
(`tsaction manifest`, under the `jsx` its tsconfig declares): every source-file
target under `main`, `module`, `browser`, `exports` and `imports` names the
emitted file -- the `.js`, or the `.jsx` for a `.tsx` under `jsx: "preserve"`
([a `.tsx` under `jsx: preserve`](../rules/ts-compile.md#a-tsx-under-jsx-preserve))
-- and every `types`, `typings` or `exports` `types` condition names the
`.d.ts`, key order kept, so an `exports` condition map is read in the order it
was written. A member that sets no `type` is ESM.

| the member's manifest says | the link's manifest says |
|---|---|
| `exports: {".": "./src/index.ts"}` | `exports: {".": "./src/index.js"}` |
| `exports: {"./wire": "./src/wire/index.ts"}` | `exports: {"./wire": "./src/wire/index.js"}` |
| `exports: {".": {"types": "./src/index.ts", "default": "./src/index.ts"}}` | `{"types": "./src/index.d.ts", "default": "./src/index.js"}`, in that order |
| `exports: {"./icons/*": "./icons/components/*.tsx"}` | `exports: {"./icons/*": "./icons/components/*.js"}` |
| the same, the member's `ts_config` declaring `jsx = "preserve"` | `exports: {"./icons/*": "./icons/components/*.jsx"}` |
| `main: "./schema.ts"`, no `exports` | `main: "./schema.js"` |
| `exports: {"./theme.css": "./theme.css"}` | unchanged: no source file |

tsc maps a `.js` or `.jsx` target to the `.d.ts` beside it, node runs the `.js`
and vite transforms the `.jsx`, so one manifest serves the type check and the
run: `import { frame } from
"@acme/canvas-sdk/wire"` resolves for tsgo to `src/wire/index.d.ts` and for
vitest to `src/wire/index.js`, both under the tree. The tree's root is the
member's directory under `bazel-bin`, where the compiling target's outputs hang
off, whichever directory holds that target. A member whose directory holds no
`package.json` with a `name` gets a comment in the hub and no view; two members
of one name, or one directory linked under two names, fail the extension.

The tree holds the member's data srcs too, at their package-relative paths
beside the `.js` that reads them: a member whose module imports `./banner.json`
answers `import { tagline } from "shared"` from the tree alone. The
`package.json` src is staged as written, and the manifest as built takes its
place where a reader holds the emit: the store tree copies it as
`package.json`, and a dependent's program root lays it at the member's path
over the src. A test inside the member imports the member by name -- a
self-reference, which tsc, node and Vite resolve through the nearest
`package.json`'s `name` and `exports` -- with the member's `ts_compile` as its
dep and no link: tsgo reaches the `.d.ts` through the manifest as built in the
program root; at run time the runfiles hold the src as written, so the name
lands on the source its `exports` name, which vitest transforms as the
checkout's vitest does and the node:test hook maps to the compiled sibling
([Files at Run Time](../rules/ts-test.md#files-at-run-time)). A test that
reads its `package.json` as data reads what the checkout has. The
link target forwards the view's `TsInfo` and `NpmPackageInfo`; a consumer
reaches the member's files in the store, as it reaches any npm package's.
`ts_test` names the closure's workspace members in `test.server.deps.inline`:
vitest runs a module under
`node_modules` in node unless a pattern names it, and a member's emitted `.js`
keeps its sources' extensionless relative imports, which node's loader rejects
and vite resolves; under pnpm a linked member is inlined because its realpath
lies outside `node_modules`.

The tree holds the member's own files and nothing outside them, so a member's
file names another package by its package name. A relative path that leaves
the member (`../../../../apps/frontend/generated/x.ts` from
`packages/bridge/src/generated/`) resolves under pnpm alone, where
`node_modules/<name>` is a symlink to the member's directory and node resolves
the importer to its real path first. Here the member is its files in the store
tree, and tsgo and both runners resolve a package's file at its realpath there,
so the path lands beside the other store trees, where the file is not:
the run fails with `Cannot find module`, and tsgo reports `TS2307`
in the member's `.d.ts` under `--//ts:lib_check` and, without it, widens every
name the file re-exported to `any`. A `.ts` subpath into a member with no
`exports` map (`apps/frontend/generated/x.ts`, the shape an application package
is imported by) resolves as the member's emitted files do: tsgo maps the `.ts`
to the `.d.ts` beside it, and the runners map it to the `.js`
([`.ts` Specifiers](../rules/ts-test.md#ts-specifiers)).
`//packages/by-name-member` is the example.

## Bin Scripts

Packages with a `bin` entry in their `package.json` get a `_bin` label:

| npm package | Binary label |
|-------------|-------------|
| `vitest` | `@npm//:vitest_bin` |
| `esbuild` | `@npm//:esbuild_bin` |
| `oxlint` | `@npm//:oxlint_bin` |

Use these as `executable` targets or as `tools` in custom actions. The hub cannot
know whether a package has a bin without downloading it, so each `<label>_bin`
alias is declared unconditionally and resolves only when something asks for it.
Asking for one on a package with no bin script is an error at that point, not at
load time. The bin chosen follows npm's own convention: the entry named after the
package, else the only one.

## npm Aliases

A dependency declared under a different name than the package it resolves to,
`"h3-v2": "npm:h3@2.0.1"`, gets its own label, so the name your code imports
exists as a target:

```python
deps = ["@npm//:h3-v2"]
```

An alias label is created only when no real package in the lockfile already
claims that name. An alias that resolves to two different packages in one
lockfile is an error, because the hub is one flat namespace.

How the alias was spelled makes no difference: `npm:h3@2.0.1` at the use site
and a `catalog:` entry that pins the same thing produce the same lockfile entry,
because pnpm resolves the catalog before it writes the file.

## Workspace Links

A `workspace:*` dependency resolves to a target in your own repository, and its
hub label carries the npm name the lockfile imports it under:

```yaml
# pnpm-lock.yaml
    dependencies:
      shared:
        specifier: workspace:*
        version: link:packages/shared
```

```python
deps = ["@npm//:shared"]     # → //packages/shared:shared, importable as "shared"
```

`import { x } from "shared"` resolves because that hub target declares the name
itself. It is a generated rule and not an `alias`: Bazel resolves an alias before
any rule implementation runs, so `ts_compile` would see no record of the name.

The target a `link:` entry points at is `//<member>:<basename>`, the
`ts_compile` Gazelle writes for the member's own `tsconfig.json`
(`//packages/shared:shared` for `link:packages/shared`), and the hub reads the
member's BUILD file to see that it declares one. That target has to be visible
to the hub repository, so `visibility = ["//visibility:public"]`. The view
forwards its providers and describes it as an npm package named by the
lockfile.

!!! warning "A member whose target is not declared gets no hub target"
    If the member's BUILD file declares no target of the member's basename, the
    hub declares nothing for that name and writes a comment saying so where the
    label would have been. `@npm//:<member>` then fails as an undeclared target
    for whatever asks for it. That covers a member with no `BUILD.bazel`, one
    whose `BUILD.bazel` declares something else (a lone `ts_config`, say), and
    a member whose program is a `tsconfig.json` above it (at `packages/`, not
    at `packages/shared/`), which gives it no target of its own. A label naming
    a target Bazel cannot resolve fails analysis for everything that reaches
    the hub, not just for the member. Give the member its `tsconfig.json` and
    run Gazelle, or write the target by hand.

A workspace member is a store tree like any other package, so a `ts_test` or
`ts_binary` that lists the importer's link target, `//:node_modules/shared`,
can import it at run time and not only type-check against it. Its own npm dependencies come along, and its
`package.json` is the member's own with source-file targets rewritten to the
emitted files, so the entry and every `exports` subpath resolve at run time as
they do for the check; see
[what a workspace member is imported as](#what-a-workspace-member-is-imported-as).
In the editor the checkout's `node_modules` holds pnpm's link to the member, and
the generated tsconfig writes no `paths` key for it.

## node_modules Targets

Every lockfile importer's package holds a `node_modules` target, its declared
npm packages linked into the store, and a `node_modules_member` per member it
links; Gazelle writes both from the lockfile's `importers:`. The root
importer's names `hoist`, the lockfile's `:node_modules/.pnpm/node_modules`;
every other importer's names `parent`, the importer above's target:

```python
load("@rules_typescript//npm:defs.bzl", "node_modules", "node_modules_member")

node_modules(
    name = "node_modules",
    deps = ["@npm//:vitest", "@npm//:react"],
    hoist = ":node_modules/.pnpm/node_modules",
)

node_modules_member(
    name = "node_modules/shared",
    member = "@npm//:shared",
)
```

`ts_codegen`, `ts_binary` and `ts_dev_server` take the importer's target and
stage its links and every store tree they reach; `ts_compile` and `ts_test`
name it in `node_modules` and resolve each npm dep along it and its `parent`s
([The Chain](../rules/node-modules.md#the-chain)). See
[Testing with vitest](testing.md) and
[node_modules](../rules/node-modules.md#the-layout).

Beside them, every lockfile's package declares pnpm's virtual store:
`npm_virtual_store(name = "node_modules/.pnpm")`, loaded from the hub's
`defs.bzl`, is one cached tree of real files per snapshot at
`node_modules/.pnpm/<key>/node_modules/<name>`, its dependency links declared
symlinks beside it, a tree per workspace member, and pnpm's hidden hoist; every
package target carries its store as `NpmPackageInfo.store`. See
[The Store](../rules/node-modules.md#the-store).

## One Repository per Package

The extension does the whole-graph analysis the lockfile text alone supports
(platform filtering, which version a bare label means, `@types` pairing, cycle
breaking, alias naming, patch routing) and declares one repository per package.
Each package reads its own `package.json` and writes its own BUILD file, so
Bazel fetches on demand, fetches independent repositories in parallel, caches
and invalidates per package, and a malformed tarball fails only its own package.
A single repository for the whole lockfile reads `bin` and `exports` out of each
extracted `package.json` to generate targets, so nothing can be emitted until
everything is downloaded.

Inside its repository a package sits under `node_modules/<name>/`, so every path
the rules write for it -- an action input, an exec path such as
`external/+npm+npm__zod__4_1_5/node_modules/zod/index.d.ts` -- carries a
`node_modules` segment. TypeScript classifies a file by that segment: under one
it is a library file, type-checked and never emitted; under none it is project
source, emit-eligible and checked against `rootDir`. The store tree is copied
from the package root.

One measurement, made while both layouts existed: building one vitest test
target from an empty output base against a 2731-package lockfile went from 392s
and 2.9 GB of `external/` to 66s and 415 MB, fetching 138 packages (vitest's
transitive closure) out of 2731. The single-repository implementation has since
been deleted, so the comparison cannot be re-run from this tree.

To count the package targets one target reaches, without building anything:

```bash
bazel query 'kind(ts_npm_package, deps(//path/to:my_test))' | wc -l
```

That is close to the set of repositories Bazel would fetch: a package present
under an npm alias name contributes a second target in the same repository. On
this repository's own lockfile `//tests/vitest:math_test` reaches 113 targets in
113 repositories.

The single-repository layout, its `npm_translate_lock` repository rule and the
`npm.translate_lock(lazy = ...)` attribute are gone.

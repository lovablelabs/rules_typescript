# IDE Setup

`ts_refresh_tsconfig` turns Bazel's build graph into the two things an editor can
read:

1. A workspace-root `tsconfig.json` whose `compilerOptions.paths` names every
   `ts_compile` package your targets reach, source directory and `bazel-bin`
   twin. The file is checked in.
2. A **tsserver plugin** that resolves the same set live, following `bazel build`
   outputs with no tsconfig reload. It is a layer on top of the generated file,
   and it needs editor configuration; the generated file needs none.

## Generated sources in native editors

For generated sources or declaration trees, set
`ts_refresh_tsconfig(tsconfig = None, generated_sources = True, deps = [...])`.
The macro rejects `generated_sources = True` with any other `tsconfig` value,
including its default: native editors need the authored solution wrapper below
to discover these projects.
Name the owning program, including its tests when they share the compiler program.
Refresh materializes the generated inputs in Bazel's output tree and installs
`<package>/.bazel/tsconfig/<target>.json`; it preserves authored configurations
and source-side generator outputs. Ignore `.bazel/` in version control.
Each installed project includes the roots observed for its target. Add new
sources to the Bazel graph and refresh before the editor checks them; authored
include patterns cannot pull sibling targets into the installed project.
Explicit generated `files` entries must name the exact selected filename and
extension. Refresh rejects an entry absent from the compiler's loaded roots,
including a missing child of a declared generated tree, and preserves the previous
editor project; ordinary compilation is unchanged.

Keep the authored program in `tsconfig.build.json`, selected by
`ts_config(name = "tsconfig", src = "tsconfig.build.json")` with `# keep` on
`src`. Gazelle reads that selected file and ignores derived editor projects.
The tracked `tsconfig.json` extends the build config, sets `files` and `include`
to empty arrays, and references `./.bazel/tsconfig/<target>.json`.
The [generated-input fixture](../../tests/integration/lsp/generated) shows this
setup. Package-local placement preserves ancestor lookup for ambient type packages.

Run `bazel run --run_validations=false --output_groups=-_validation //:refresh_tsconfig`
before opening the editor and after changes to BUILD files or inputs Bazel tracks.
The output-group flag cancels an explicit `+_validation` in a workspace rc. Use
`ibazel run` with the same arguments to rerun refresh for those changes;
`--watchfs` alone does not rebuild. Bazel owns generation and the editor's
established client owns file-change notifications. No tsserver plugin is required
for this mode.

Refresh also installs the current plugin package so an already enabled global
plugin respects the projected aliases. After upgrading the ruleset, run refresh
and restart the TS server once to load the updated plugin. Later BUILD or
generator-input refreshes use the same running editor session.

When refresh serves a generated file from a cache, Bazel replaces it within about
a millisecond. tsserver's own file watcher can miss that replacement and keep the
old declaration, for example a completion for a deleted generated member, until
the TS server restarts. Restart it if the editor shows a generated declaration
that the refreshed file no longer has.

Derived projects retain the native compiler’s effective resolution mode, including
defaults omitted from its printed configuration. The editor compiler must support
that mode and the authored options. For example, TypeScript 5.9 rejects Bundler
resolution with CommonJS modules; use a compatible editor compiler for that
program rather than changing resolution to hide the diagnostic.

Projection runs `--noEmit --traceResolution --explainFiles` when refresh requests
it. It performs semantic checking to collect missing reference-path diagnostics
and uses `--//ts:checkers` for checker threads and Bazel's CPU reservation.
Consumer semantic errors do not block projection, but generators and declaration
producers must succeed. In particular, a dependency's type error can prevent
declaration emission with `--//ts:declarations=tsgo`. Tracing retains failed
ordinary import resolutions even when their diagnostics are suppressed. It adds
output and bypasses the compiler's resolution-result cache. Refresh cost is not
measured.

Refresh rewrites aliases only where generated output resolution requires it,
preserving authored specificity and fallback order. An alias selecting an
authored file inside a generated tree uses that compiler-selected file when
relocating its candidates would hide it, including extensionless and `.js`
spellings. Changed selections need another refresh. Other authored aliases and
npm resolution stay native. Tree aliases and scalar aliases directly naming their
generated file relocate the original ordered candidates, so adding or deleting
a preferred candidate changes native fallback without project regeneration.
Other scalar aliases use the compiler's exact file selection; new source or
import choices that need exact selection require another refresh. Generated
JSON inputs use the same projection when `resolveJsonModule` enables their imports.
Refresh rejects a synthesized tree wildcard when it would outrank an overlapping
authored suffix alias. Authored aliases keep their key order, including the first
match among equal-prefix patterns such as `#x/*z` and `#x/*`. Synthesized wildcard
aliases follow authored aliases, so an equal-prefix synthetic pattern cannot replace the
authored winner. Unrelated suffix aliases retain native resolution. Refresh projects
a broad alias such as `#src/*` → `./src/*` over a generated subtree
`src/generated` for imports such as `#src/generated/value`. At the directory
boundary, refresh rejects a projection that hides an observed authored sibling
resolution: moving `generated/model` hides that import's `generated/model.ts`,
while keeping the checkout directory can revive a deleted generated index. Name
the authored file explicitly in a distinct alias, or name a file inside the tree.
Refresh checks the current compiler program; a new source or import choice
introducing a boundary sibling needs another refresh.
Generated tree aliases use canonical output paths; relative
imports and re-exports within those outputs stay in that namespace when a child
is replaced or deleted. The editor project has no workspace/output `rootDirs`
overlay through which a deleted child could resolve to a stale checkout twin.
Generated `typeRoots` also point into canonical output; authored type roots
retain native type-package lookup. Refresh rejects a relocated root when explicit
or implicit type-package lookup, or a source `/// <reference types="..." />`
directive, selects an authored package inside it, including with `types: []`.
Separate authored and generated type roots so both retain their identity.
A type root containing a generated child package or scalar declaration cannot
exclude stale checkout packages. Refresh rejects that shape before replacing
the installed project: generate the whole
type root or use an authored editor project without `generated_sources`.
A generated declaration named by a path-shaped `compilerOptions.types` entry is
compiled at its source path, so an `exclude` pattern matching that source path
drops it as well, in builds and in the editor project alike.

Refresh rejects compiler-reported relative edges between authored sources and
generated output in either direction, including explicit paths through
`bazel-out`. Installation binds output paths outside the checkout, where those
relative imports cannot retain their selected files. Use a
`compilerOptions.paths` alias for the crossing. Relative edges within authored
sources or within generated output remain native, including authored imports of
an emitted library whose build resolves through declarations beside its sources.
Refresh also rejects relative edges that need the build's `rootDirs` overlay and
compiler-reported unresolved relative references into a declared generated tree,
including when the canonical child is absent. For scalar reference paths, this
includes extensionless references whose compiler-reported candidates name a
generated file. A tsconfig cannot redirect those edges while excluding a checkout
twin; use an alias or an authored editor project without `generated_sources`.

Preventing stale checkout sources from resolving is best effort, even after
refresh. With current supported compilers, missing
`/// <reference path="..." />` targets may appear only as suppressible diagnostics;
suppression can leave refresh without the reference facts needed to reject the
edge. This guarantee can be tightened when a supported compiler release exposes
those facts independently of diagnostic suppression.

Refresh also rejects first-party package `imports` or `exports` that reach
generated output through checkout paths, including missing targets with stale
checkout twins and package fallback after all matching `paths` targets fail.
Use a `compilerOptions.paths` alias into generated output or an
authored editor project without `generated_sources`. Package imports already
resolving within canonical output retain native resolution.

Generated refresh also rejects a workspace member reached through an npm link
when its dependency closure contains original generated compiler inputs, including
scalar sources, JSON inputs, and generator declaration trees. The build uses the member's store
copy, while the editor follows pnpm's checkout link; refresh has no mapping between
those identities. This is a whole-member restriction, even when the current
compiler listing does not select the generated input. The refusal names the
member link and a generated producer witness before installation. Authored members
remain supported when only their emitted declarations or manifests are generated.
Direct generated inputs outside workspace-member links retain the projection
support described above; ordinary builds remain supported in either case.

Refresh materializes the producer-owned `package.json` beside generated inputs so
NodeNext retains their module format on a clean refresh. An effective authored
package scope with `imports` or `exports` enclosing relocated generated inputs is unsupported:
its relative namespace changes, including queries the compiler has not observed.
Refresh rejects it before publishing. Generated package scopes retain their
producer path and contain only generated members in the compiler-loaded program,
whether scalar or tree outputs. Config-only inputs and declared but unloaded
files do not count as program members; compiler-loaded JSON counts even when it
also supplies configuration. Refresh rejects scopes mixed with retained authored
members, including files whose module format would change, and renamed producer manifests. Authored
siblings outside the scope do not conflict. A nearer complete generated package
scope owns its private imports and self-references, so an outer authored scope
does not block refresh. Authored inputs keep their original scope.

Generated sources may require a wider `rootDir`, including for `noEmit` projects.
Declarations and config artifacts do not widen it. When both output directories
are explicitly disabled, refresh conservatively includes imported generated
sources in containment. A nonempty containment set gives the read-only editor
project a filesystem-volume `rootDir`, preserving `composite` while containing
workspace aliases on that volume and canonical generated paths. With active or
inherited output directories, the compiler listing must establish containment
eligibility: imports with unknown external-library membership are
rejected unless they are also compiler-listed program roots. Authored and unmoved
inputs do not need this proof.

When source containment requires widening, refresh rejects active or inherited
`outDir` and `declarationDir`: those options make package resolution depend on
`rootDir`, even for unobserved queries. This can reject direct-source imports that
happen to be unaffected. Use an editor configuration without emission output
directories or an authored editor project without `generated_sources`.
Projects retaining emission output directories preserve their `rootDir` and resolution context; open these projects through the physical workspace path, because unrelated workspace aliases are not guaranteed to contain canonical generated sources.
Output-directory support also requires unchanged containment of the effective
project file by every declared first-party package scope with `imports` or
`exports`. Moving the config inside a package can enable output-to-source remapping
without widening `rootDir`, including for declaration-only generated input.
Refresh rejects that context change before publishing, even when the current map
uses direct-source targets that happen to be unaffected. Configs without emission
output directories and scopes with unchanged containment remain supported. Npm
resolution through the declared `node_modules` chain is excluded from this local
output-to-source remapping rule.

Generated tree aliases retain native `moduleSuffixes` selection, including
fallback and deletion. Exact file projection rejects nonempty suffixes because the compiler
applies them again to the selected filename, potentially selecting another file
or failing. Aliases already pointing into canonical output need no exact
projection and retain native suffix selection.

Generated projection also rejects generated inputs from external repositories and
external authored sources the compiler reads, including `.d.ts` and `emit = False`
dependencies. This applies even when the consuming target and its authored config
are local. Ordinary builds can still consume these inputs.

Generated inputs must share the editor project's Bazel output root. Refresh
rejects local producers built in another configuration, such as through a
configuration transition, before replacing the installed project. Use an authored
editor project without `generated_sources` for those programs; ordinary builds
and type checking remain supported.

It also rejects external authored configs, conflicting selections for an exact
alias, and aliases needing exact projection
under `node16` or `nodenext` resolution. Exact paths can hide failed imports in
another importer's resolution mode. Relocated original candidates, including
explicit scalar filenames, and unprojected authored aliases retain native
resolution in those modes. Rejection leaves the installed project intact.
Use an authored editor project or refresh without `generated_sources` for
unsupported programs; ordinary builds and type checking remain supported.

Native-editor replacement, deletion, and recovery behavior is not measured.
Acceptance requires one running editor with an unchanged consumer observing
all three through its established file-watching adapter.

## Setup

Declare the target once, in your root `BUILD.bazel`:

```python
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

An aspect walks `deps` from each entry, so listing a target covers everything it
depends on. Two constraints:

- **`deps = []` is the attribute default, and it reaches nothing.** The result is
  a `tsconfig.json` with an empty `paths`: no packages, no aliases.
- **`deps` obeys visibility**, so a package-private `ts_compile` target cannot be
  listed here. Gazelle writes `visibility = ["//visibility:public"]` on the
  targets it generates. A `ts_test` carries its own program and is listed itself, under its own `visibility`; the targets over
  this list are testonly for it. Hand-written private targets are covered by
  [Complete coverage for the resolution map](#complete-coverage-for-the-resolution-map).

Then run it:

```bash
bazel run //:refresh_tsconfig
```

That writes, into the source tree:

| Path | What it is |
|---|---|
| `tsconfig.json` | Compiler options and the `paths` map; checked in |
| `.bazel/tsserver-hook-data.json` | The same graph facts, in the shape the plugin reads |
| `.bazel/node_modules/@rules_typescript/tsserver-plugin/` | The tsserver plugin, as a package tsserver can load by name |
| `.bazel/tsserver-hook.js` | A preload variant for a client that resolves through the public `ts.resolveModuleName`; see [What the preload does not reach](#what-the-preload-does-not-reach) |
| `.bazel/tsserver-hook-resolver.js` | The map builder both front-ends share |
| `.bazel/tsserver-hook-worker.js` | Its background worker |

The target copies those files into the source tree, so it runs only under
`bazel run`.

`tsconfig` (default `"tsconfig.json"`) is where the generated config lands.

Add `.bazel` to `.bazelignore`, so Bazel never reads the plugin's files as a
package. This repository's own `.bazelignore` starts with that line.

!!! warning "It replaces the file at `tsconfig` wholesale"
    A migrating repository already has a root `tsconfig.json`, and the first
    `bazel run //:refresh_tsconfig` overwrites it: `include`, `baseUrl`,
    `module` and every other option in it, not only `paths`. The generated file
    is a complete config and carries nothing over from yours.

    By default, Gazelle discovers the program through `tsconfig.json`.
    Renaming it without selecting the new file through a kept `ts_config.src`
    leaves no program there, so Gazelle withdraws the rules it wrote. The
    [generated-source setup](#generated-sources-in-native-editors) uses that
    explicit selection to retain the authored program. For the default setup, set
    `ts_refresh_tsconfig(tsconfig = "tsconfig.bazel.json")` and `extends` the
    generated file from yours; see
    [Extending the Generated File](#extending-the-generated-file).

### Extending the Generated File

A repository that keeps its own `tsconfig.json` points the generator at another
name and extends it:

```python
ts_refresh_tsconfig(
    name = "refresh_tsconfig",
    test = True,
    tsconfig = "tsconfig.bazel.json",
    deps = ["//src/app", "//src/lib"],
)
```

```json
{
  "extends": "./tsconfig.bazel.json",
  "compilerOptions": { "paths": { "@/*": ["src/*"] } }
}
```

Three edits make that build:

1. **The `ts_config` needs the base in `deps`.** Gazelle writes
   `ts_config(name = "tsconfig", src = "tsconfig.json")` and fills `deps` for
   an `extends` naming another `tsconfig.json` by relative path. A base of
   another name gets no entry, and every target fails with
   `error TS5083: Cannot read file '.../tsconfig.bazel.json'.` Add the file
   with a `# keep`, since `deps` is
   [Gazelle's](../gazelle/directives.md#attributes-gazelle-owns):

    ```python
    ts_config(
        name = "tsconfig",
        src = "tsconfig.json",
        deps = ["tsconfig.bazel.json"],  # keep
        visibility = ["//visibility:public"],
    )
    ```

2. **An option the root block cannot hold puts every package on the nested
   list.** Gazelle wires your file onto every target, so a `"module": "ESNext"`
   in it disagrees with the root block's `Preserve` for every package with
   targets, and the first `bazel run //:refresh_tsconfig` fails instead of
   writing:

    ```
    ts_refresh_tsconfig: the nested_tsconfigs list does not match what the
    graph needs.
      add:    src/app/tsconfig.json, src/lib/tsconfig.json
    ```

    List them in `nested_tsconfigs` ([Nested Tsconfigs](#nested-tsconfigs)), or
    drop the option from your file.

3. **Each nested package exports its `tsconfig.json`.** The staleness
   `diff_test` for a nested entry names `//<pkg>:tsconfig.json`, and the
   generated file is one Gazelle skips, so nothing declares it. Analysis fails
   with `no such target '//src/lib:tsconfig.json'` until the package's BUILD
   carries `exports_files(["tsconfig.json"])`, as `vite/BUILD.bazel` and
   `tests/compiler_options/BUILD.bazel` do here.

With those three and `.bazel` in `.bazelignore`, `bazel build //...` and the
staleness tests pass. A tsconfig with `"baseUrl"` in it fails for another
reason; see
[Option 'baseUrl' has been removed](../guides/troubleshooting.md#option-baseurl-has-been-removed).

### Excluding Foreign TypeScript

The generated config leaves `include` at `**/*`, so `tsc` walks every `.ts` in
the repository, including trees outside this module's build graph: a nested Bazel
module, a workspace listed in `.bazelignore`, a vendored example. Nothing in
`deps` names those files, so they are checked under the wrong `compilerOptions`
and their errors are noise. `extra_exclude` adds globs to the generated
`exclude`:

```python
ts_refresh_tsconfig(
    name = "refresh_tsconfig",
    test = True,
    extra_exclude = ["**/e2e", "**/examples"],
    deps = ["//apps/web", "//packages/design-system"],
)
```

Anchor each entry with `**/`, the way the built-in exclusions
(`**/bazel-*`, `**/node_modules`, `**/dist`, `**/build`, `**/.next`,
`**/.nuxt`, `.bazel`) are.

### Nested Tsconfigs

One `compilerOptions` block cannot serve a target that turns `strict` off beside
one that leaves it on, or a target naming a `lib` its `target` does not imply.
An editor resolves a file to a program by directory, so such a package needs
its own `tsconfig.json` next to its sources, and the root has to stop claiming
those files.

`ts_refresh_tsconfig` generates those files, and you declare which packages get
one:

```python
ts_refresh_tsconfig(
    name = "refresh_tsconfig",
    test = True,
    nested_tsconfigs = ["apps/worker/tsconfig.json"],
    deps = ["//apps/web", "//apps/worker"],
)
```

The set is computed from what each target names: the `tsconfig` it compiles
under, and `allowJs` for a target with JavaScript srcs where the root block does
not set it. A package whose targets name a tsconfig of their own gets its own
program; one whose targets name none stays in the root's. A package whose
targets name the `tsconfig.json` in their own directory -- every package
Gazelle writes -- has its program already, that file: nothing is generated for
it, and the root program excludes the files it covers.

The rule fails when the declared list disagrees with the graph, in either
direction, and the message names what to add or remove. The list is declared
because `glob()` does not cross a package boundary, and a leftover entry would go
on owning its subtree in the editor. Each entry gets its own staleness
`diff_test`.

Each generated file `extends` the root and the tsconfig the package's targets
name, root first so the package's file wins. Inherited `paths` are not
re-resolved, so the root's aliases still work from down there; `include` and
`exclude` are re-resolved against the extending file, so they are written out.
`noEmit`, `composite`, `incremental`, `rootDir` and `files` are pinned in the
file itself, since a tsconfig inherited whole would emit into your source tree
and reject files outside one target's `rootDir`.

A package whose targets set the same option to different values has no
representation, since one directory cannot hold both answers. That is an error
naming both targets; move one target into its own package.

**Two different `tsconfig` files in one package is the same error.** TypeScript
applies an `extends` array later-wins, so listing both would let one's keys
replace the other's for both targets' sources. A package gets at most one, from
whichever of its targets name one.

A target in that package naming no `tsconfig` inherits that file in the editor,
and does not in the build: the rule's baseline (`strict`, `module: Preserve`,
`target: es2022`, `jsx: react-jsx`, `skipLibCheck`, `esModuleInterop`) is all
it gets there, which is what the root block holds. Give the odd target the same
`tsconfig`, or its own package, when that is not close enough.

### Bare Specifiers for First-Party Packages

Every package the aspect reaches gets a `paths` key of its own in the root
config -- `@/<rest>/*` for a package under `src/`, the package path otherwise,
plus the bare key once the package has an index file -- mapped to the source
directory and its bazel-bin twin. A pnpm `link:`/`workspace:` dependency
imported by its package name gets no key: the checkout's `node_modules` holds
pnpm's link to the member, which is where the build resolves it too, through the
hub's view of the member.

### npm Packages

The generated config names no npm package. TypeScript resolves a bare specifier
by walking the checkout's `node_modules` from the importing file, as tsgo walks
the importer chain a build stages, so `pnpm install` is the editor's npm setup: the tree
it installs is the lockfile's, which is what the build resolves too. A
`@types/*` package, an `exports` subpath and a `types` entry naming a package
resolve the same way. A package your targets declare and the checkout does not
hold is `TS2307` in the editor until the next `pnpm install`.

### Staleness Test

`test = True` adds a `diff_test` named `<name>_test` that compares the
checked-in `tsconfig.json` against the one the graph currently implies:

```bash
bazel test //:refresh_tsconfig_test
```

It fails whenever a dependency edit changes what the IDE should see:

```
tsconfig.json is stale: run `bazel run //:refresh_tsconfig`.
```

Turn it on once the file is checked in.

## Complete Coverage for the Resolution Map

`deps` is a rule attribute, so it reaches only what this workspace's visibility
lets a rule name. An aspect propagates along the dependency edges a build
already has and creates none, so it needs no grant. Two lines in `.bazelrc` turn
that on for every build:

```
build --aspects=@rules_typescript//ts/private:tsconfig_aspect.bzl%tsconfig_aspect
build --output_groups=+ide_fragments
```

Each compiler target that contributes a package writes a
`<target>.tsconfig-fragment.json` beside its other outputs in `bazel-out`.
Wrappers forward those fragments, and the resolver merges them into the map. `+group` is
additive, so this composes with `--output_groups=+_validation` and with anything
a command line adds, and any ordinary `bazel build` refreshes the fragments.

**Both lines are optional.** Without them nothing writes fragments and the plugin
works from `.bazel/tsserver-hook-data.json` alone. Fragments augment that file;
they never replace it, and every key it resolved wins over a fragment that
disagrees.

### What Fragments Cover

| | Covered by | Reaches package-private targets |
|---|---|---|
| `ts_compile` source roots | fragments, and the data file | yes, via fragments |

npm packages are in neither: TypeScript resolves them through the checkout's
`node_modules` ([npm Packages](#npm-packages)).

The checked-in `tsconfig.json` does not change either. It stays what
`refresh_tsconfig` generates from `deps`, which is what a fresh clone, a plain
`tsc` run and every editor read. Fragments reach the plugin only.

### Cost

- **Each fragment carries its target's whole closure**, at the cost of bytes:
  any one fragment is a complete answer for its own subgraph, which makes a
  partially built `bazel-out` usable.
- **A deleted or renamed target leaves its fragment behind**, because nothing
  cleans `bazel-out`. The resolver opens fragments only under directories the source
  tree still has a BUILD file for, and nothing enters the map unless the path it
  names exists on disk, so a stale fragment contributes nothing. `bazel clean` is
  not needed.

## Ambient Types in the Editor

The editor is more permissive than the build in one place. `ts_compile` writes
`types` from the tsconfig's entries or, when it sets neither `types` nor
`typeRoots`, from the direct `@types/*` deps' names, and a tsconfig that sets
`typeRoots` admits what those roots hold, so a global reaches a target because
that target asked for it. The editor's root program has one `compilerOptions`
block for the whole workspace and no `types` key, so TypeScript includes every
`@types/*` package under the root `node_modules/@types`. A file using `process`
type-checks in the editor as soon as `@types/node` is installed, and then fails
`bazel build` with `TS2304` until the target asks for it: `node` in its
tsconfig's `types`, or `@types/node` among its direct deps when the tsconfig
sets neither `types` nor `typeRoots`.

Narrowing that per target would need a tsconfig per target, and a package only
gets its own program when its targets name one
([`nested_tsconfigs`](#nested-tsconfigs)).

Treat `bazel build` as the authority, and declare ambient packages up front:
`"types": ["node"]` in the tsconfig every package of a tree extends does that
in one line.

## Editor Configuration

The generated `tsconfig.json` needs no editor setup; every editor already reads
it, and it is what makes a Bazel-built declaration resolve in a fresh clone.
The rest of this section is for the plugin, which adds live resolution on top.

tsserver loads a plugin by name from a probe location. The plugin is installed as
a package under `.bazel/node_modules/`, so the probe location is `.bazel` and the
name is `@rules_typescript/tsserver-plugin`. Every recipe below is those two
facts in one editor's spelling.

### VS Code

`.vscode/settings.json`:

```json
{
  "typescript.tsserver.pluginPaths": [".bazel"]
}
```

VS Code passes no `--globalPlugins`, so the plugin has to be named in the config
the editor is using as well; the generated `tsconfig.json` names it, and
`bazel run //:refresh_tsconfig` keeps it there. Do not add the entry by hand to
a config the macro owns: the next refresh rewrites the file whole and drops it,
and tsserver logs and ignores a plugin it cannot load, so the only symptom is
unresolved imports.

A workspace whose editor config is its own file (`ts_refresh_tsconfig(tsconfig
= "tsconfig.bazel.json")` with a hand-written `tsconfig.json` that `extends` it)
inherits the entry through `extends` and needs nothing either.

Restart the TS server: `Cmd+Shift+P` → `TypeScript: Restart TS Server`.

### Neovim (nvim-lspconfig with typescript-language-server)

```lua
require('lspconfig').ts_ls.setup({
  init_options = {
    plugins = {
      { name = "@rules_typescript/tsserver-plugin", location = ".bazel" },
    },
  },
})
```

`typescript-language-server` turns its `plugins` option into tsserver's
`--globalPlugins` and `--pluginProbeLocations`, so no `compilerOptions.plugins`
entry is needed with it.

### Neovim (coc-tsserver)

`coc-settings.json`:

```json
{
  "tsserver.globalPlugins": [
    { "name": "@rules_typescript/tsserver-plugin", "location": ".bazel" }
  ]
}
```

### Emacs (lsp-mode)

```elisp
(setq lsp-clients-typescript-plugins
  (vector (list :name "@rules_typescript/tsserver-plugin"
                :location ".bazel")))
```

### tsserver Directly

Any client that spawns tsserver itself takes the two flags:

```
--globalPlugins @rules_typescript/tsserver-plugin
--pluginProbeLocations /abs/path/to/workspace/.bazel
```

## Coding Agent Harnesses

A coding agent that reads TypeScript usually runs its own language server, not
an editor's. Claude Code installs `typescript-language-server` and `typescript`.

**The generated `tsconfig.json` needs no configuration.** It is a checked-in
file with a `paths` map, which every TypeScript tool reads. An agent's language
server resolves a Bazel-built `.d.ts` through it to the real declarations, not
to `any`; a nonexistent member on an imported symbol is still an error.
`bazel run //:refresh_tsconfig` keeps the file current, and the
[staleness test](#staleness-test) asks for it.

**The plugin needs a configurable server.** If the harness exposes LSP
`initializationOptions`, pass the `plugins` entry from the
[nvim-lspconfig recipe](#neovim-nvim-lspconfig-with-typescript-language-server);
most harnesses run `typescript-language-server`. If it does not, the plugin
cannot be reached and the `tsconfig.json` is the whole answer.

Two facts about `NODE_OPTIONS`:

- **`NODE_OPTIONS` is not a way in.** It propagates into the forked tsserver, so
  the preload does load there, but loading is not the same as taking effect; see
  [What the preload does not reach](#what-the-preload-does-not-reach).
- **A relative path in `NODE_OPTIONS` kills the process.** Node resolves
  `--require ./x.js` against the process's cwd; from any other directory the
  process fails to start: `Cannot find module`, with
  `requireStack: [ 'internal/preload' ]`, exit 1. Absolute paths only.

To check what a harness gives you, ask its language server for
diagnostics on a file importing a Bazel-built package. `TS2307 Cannot find
module` before `bazel run //:refresh_tsconfig` and no diagnostic after it means
the `tsconfig.json` path works. `TSSERVER_HOOK_DEBUG=1` in the server's
environment makes the plugin report on its stderr whether it loaded and how many
entries its map holds.

### What the Preload Does Not Reach

`.bazel/tsserver-hook.js` patches the `typescript` module's exported
`resolveModuleName`. That reaches a client which builds a `LanguageService` host
itself and routes resolution through the public API. It does not reach a
standalone tsserver process, and every editor above spawns one.

Two measured facts. `lib/tsserver.js` loads its bundle as
`require("./typescript.js")`, which the preload's matcher does not accept, so the
patch never installs. With the matcher widened, a real tsserver still reports
`TS2307` for the same import: the language service resolves through its
`LanguageServiceHost`, not through the export. The plugin decorates that host.

## How It Works

The plugin is TypeScript's equivalent of Go's
[GOPACKAGESDRIVER](https://jayconrod.com/posts/125/go-editor-support-in-bazel-workspaces),
with one difference: it never runs Bazel. Everything Bazel knows arrives
through `.bazel/tsserver-hook-data.json`, which `refresh_tsconfig` wrote at
analysis time. A long-lived editor process asking the Bazel server for anything
would sit on the same lock a build wants.

1. **Worker thread** reads `.bazel/tsserver-hook-data.json` (the `ts_compile`
   package list) and turns it into a module-name → declaration-path map
2. **Internal packages** resolved from `bazel-bin` (`.d.ts` after a build
   emitted them) or the source tree (`.ts` before one)
3. **Fragments**, if the `.bazelrc` lines above are in place, add the packages
   of every target the aspect reached, including the ones no rule may name. One
   target built in two configurations writes two fragments, deduplicated by
   label with the first config root in sorted order winning, so the merge does
   not depend on what `bazel-out` holds
4. **Directory watchers** watch the graph data file's parent and canonical output
   directories and their parents, renewing watches after directory replacement or
   recreation. Changes to graph data, declarations, or fragments rebuild the map.
   npm packages and path aliases are not in the map: TypeScript resolves both
   itself, through the checkout's `node_modules` and the tsconfig's `paths`

The main thread is never blocked: the worker builds the map off-thread and posts
it back. tsserver returns "unresolved" briefly on first load, then resolves when
the worker completes.

### Resolution Priority

For packages resolved through the worker map, including fragment entries, the
worker tries `index.d.ts`, `index.ts`, and `index.tsx` in that order. For each
filename, it checks `bazel-bin` before the checkout.

Exact package aliases written by refresh retain the declared entrypoint's
provenance: authored entrypoints resolve in the checkout, and generated-only
entrypoints resolve in the output tree. TypeScript applies native extension
substitution within that namespace. Generated-only entrypoints have no checkout
fallback.

npm packages are not in the map: TypeScript resolves them itself through the
checkout's `node_modules`.

### What a Build Provides

Authored first-party entrypoints resolve without `bazel build`; generated
entrypoints require their producer to run. A build adds the `.d.ts` of every
package a dependent's compile read -- of every package with
`--output_groups=+declarations`
([Which Tool Emits the Declarations](../rules/ts-compile.md#which-tool-emits-the-declarations))
-- and, with the aspect enabled, the fragments naming the packages `deps` could
not reach.

npm resolution is bounded by the checkout's `node_modules`: a package the
lockfile gained resolves in the editor after the next `pnpm install`.

## Debugging

Set `TSSERVER_HOOK_DEBUG=1` in the environment the language server starts in.
The plugin and its worker then report on the server process's stderr: whether the
plugin loaded, which project it decorated, how many entries the map holds, and
each invalidation. It is the server's stderr and not the tsserver log, so where
it surfaces depends on the client.

## Debugging Tests in VS Code

To attach a debugger to vitest running inside the Bazel sandbox:

```python
ts_test(
    name = "my_test_debug",
    srcs = ["my.test.ts"],
    deps = [":my_lib", "@npm//:vitest"],
    tags = ["manual"],
    env = {"NODE_OPTIONS": "--inspect-brk=9229"},
)
```

```bash
bazel run //path/to:my_test_debug
```

Vitest pauses before executing, waiting for a debugger on port 9229. Attach VS Code via "Attach to Node Process" or use `chrome://inspect`. Source maps are configured automatically.

Gazelle lists every test file the program owns in the generated `ts_test`,
whatever other rule names it, so `my.test.ts` stays tested by `bazel test
//...` while this `manual` target holds it too. Delete the target when the
session is over.

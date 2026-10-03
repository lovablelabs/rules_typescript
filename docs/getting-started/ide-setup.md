# IDE Setup

`ts_refresh_tsconfig` turns Bazel's build graph into the two things an editor can
read:

1. A workspace-root `tsconfig.json` whose `compilerOptions.paths` names every
   `ts_compile` package your targets reach, source directory and `bazel-bin`
   twin. The file is checked in.
2. A **tsserver plugin** that resolves the same set live, following `bazel build`
   outputs with no tsconfig reload. It is a layer on top of the generated file,
   and it needs editor configuration; the generated file needs none.

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

    Move the generator, not your file. Under Gazelle the file keeps its name:
    a directory is a package because it holds a file named `tsconfig.json`,
    which tsgo lists, and every target in it names the `ts_config` over that
    file. Rename it and the next run finds no program there: every rule
    Gazelle wrote is withdrawn, and the run names the BUILD file. Set
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
4. **File watcher** watches the graph data file, and `bazel-bin` recursively for
   new `.d.ts` and new fragments; a change to either rebuilds the map. npm
   packages and path aliases are not in the map: TypeScript resolves both
   itself, through the checkout's `node_modules` and the tsconfig's `paths`

The main thread is never blocked: the worker builds the map off-thread and posts
it back. tsserver returns "unresolved" briefly on first load, then resolves when
the worker completes.

### Resolution Priority

1. `.d.ts` in `bazel-bin` — fast, precise (available once a build emitted them)
2. `.ts` source file — always available, slower for tsserver to process

npm packages are not in the map: TypeScript resolves them itself through the
checkout's `node_modules`.

### What a Build Provides

First-party resolution works without `bazel build`, since the source `.ts` files
are always on disk. A build adds the `.d.ts` of every package a dependent's
compile read -- of every package with `--output_groups=+declarations`
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

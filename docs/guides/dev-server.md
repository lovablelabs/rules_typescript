# Dev Server

`ts_dev_server` starts a dev server for a TypeScript application. `bazel run
//src/app:dev` builds the target once. The server then transforms first-party
source in memory, so a save reaches the browser without a Bazel build.

oj 0.2.16 is the default implementation at `@rules_typescript//oj:dev_server`.
Vite is optional: set `server = "@rules_typescript//vite:dev_server"`.
Other implementations return `DevServerInfo`; see
[Bringing your own server](#bringing-your-own-server).

## Default oj server

oj and oj_server are pinned to 0.2.16 and built from Rust source. Their V8
dependency uses an upstream static library fetched by Bazel with a pinned
SHA256 digest; V8 itself is not compiled from source here. The native
binary reads the generated Vite-format config. Its command line supplies the
serve root and port; the launcher supplies its cache environment. oj supports
opening the browser and provides native React Fast Refresh. Leave
`react_refresh = False` for oj; that attribute installs Vite's React plugin.

The oj provider does not consume `root` or `cacheDir` from the generated config.
For Vite framework plugins, select `@rules_typescript//vite:dev_server`
explicitly and follow the setup below.

## Setup

Gazelle creates a `dev` target for a tsconfig program with non-test sources
and a package index.html or listed main.ts[x] or app.ts[x]. Gazelle manages
`entry_point`, `node_modules`, `plugin` and `visibility`, and removes stale
generated dev targets. It preserves `server`, `port`, `host`, `open` and
values protected by `# keep`.

The examples and plugin details below select Vite explicitly. A hand-written
target must name the application's `node_modules` tree; without it startup fails:

```
ts_dev_server: @@//src/app:dev has no node_modules attr, so the app's own
dependencies are not in runfiles.
```

```python
load("@rules_typescript//ts:defs.bzl", "ts_dev_server")
load("@rules_typescript//npm:defs.bzl", "node_modules")

node_modules(
    name = "node_modules",
    deps = [
        "@npm//:vite",
        # every npm package the app imports, too — see below
    ],
    hoist = "//:node_modules/.pnpm/node_modules",
)

ts_dev_server(
    name = "dev",
    server = "@rules_typescript//vite:dev_server",
    entry_point = ":app",
    node_modules = ":node_modules",
    port = 5173,
    plugin = "@rules_typescript//vite:vite_plugin_bazel",
)
```

Vite itself comes from that tree; the rule does not fetch it.

```bash
bazel run //src/app:dev     # start it
ibazel run //src/app:dev    # same, plus codegen rebuilds and config-aware restarts
```

Open `http://localhost:5173/`. The application root is the package containing
`ts_dev_server`, even when its `entry_point` belongs to another package. Put the
dev-server target beside the application's `index.html`. Bazel outputs and npm
resolution still use the workspace root. The selected server runs from the
application directory.

On the optional Vite path, application environment files are read from that
application directory. Move these files, including mode-specific and `.local`
variants, beside the dev-server target if they lived at the workspace root.

For oj, move native `oj.config.*` files beside the dev-server target. A file
left at the workspace root is no longer loaded, so its settings, including deny
rules, no longer apply. Rewrite workspace-relative deny patterns for the new
dev-server root. Patterns containing `/` match root-relative or absolute paths.
For files outside that root, use absolute patterns matching real,
symlink-resolved paths. A native `server.fs` block replaces the generated
filesystem settings. Explicitly allow the workspace, the real `bazel-bin`
directory and required Bazel `node_modules` paths alongside deny rules.

For a hand-written Vite config with `root` below the Bazel workspace, pass the
absolute Bazel workspace directory as `workspaceRoot` to `bazelPlugin`.
It controls source-to-output mapping and adds that directory to `server.fs.allow`.
The default `bazel-bin`, a relative `bazelBin`, and a relative `nodeModules` path
resolve from `workspaceRoot` when set. Absolute paths stay unchanged.
When omitted, the plugin uses Vite's `root`. Generated configs set it explicitly.

## Bringing Your Own Server

`ts_dev_server` takes a `DevServerInfo`, and the implementation is a per-target
choice: `server` names any rule returning the provider, and the launcher hands
it the same generated config. A server shipping as an npm package sets
`server_in_tree` (a path under the importer's `node_modules`, reached through
the package's link); a native binary sets `server_binary`. Exactly one. A field a server declares it does not read is an
analysis-time error on a target that sets the attr reaching it, naming both, so
switching implementations cannot silently drop a setting. The provider is not
exported from `@rules_typescript//ts:defs.bzl`: it loads from
`@rules_typescript//ts/private:providers.bzl`, a path
[COMPATIBILITY.md](../compatibility.md#volatile) lists as volatile. Its eight
fields, with the values the optional Vite server returns for each, are in
[Providers](../rules/providers.md#devserverinfo).

## What Is Served from Where

| | a production bundle | `ts_dev_server` |
|---|---|---|
| first-party `.ts` | Bazel compiles it; the bundler reads `bazel-bin` | served as source, transformed by the server in memory |
| `ts_codegen` output | from `bazel-bin` | from `bazel-bin` |
| npm packages | the importer's `node_modules` | the importer's `node_modules`, linked in at the workspace root |
| imported assets, data srcs, passthrough `.d.ts` | from `bazel-bin` | from `bazel-bin` |

For Vite, declared asset selection applies to module imports, including
`?raw`, `?url` and relative references from published CSS. Literal browser URLs
such as `<img src="/logo.svg">` retain Vite's static lookup under the live
application root. Use an asset import to obtain a URL for a declared generated
File; declaring it does not remap literal browser URLs.
Emitted asset URLs use Vite's native static serving and missing-file behavior.

With Vite's Bazel plugin, generated modules use their declared Files even when
a checkout copy exists.

### How a Bare npm Specifier Resolves

Vite has no search-path option: it resolves `import "zod"` by walking up from the
importer looking for a `node_modules` directory, and above a checked-in source
file there is none; the importer's `node_modules` is a Bazel output elsewhere.
(`resolve.modules` is a webpack option; Vite ignores it.)

The launcher links that directory in as `<workspace>/node_modules` when the dev
server starts, and removes the link on Ctrl-C. Every resolver then finds the
packages by that walk, and a package's own imports from its realpath in the
store, including the two no plugin reaches:

- **SSR externalisation.** Whether a package is external is decided on the raw
  specifier before the plugin container sees it. A package that does not resolve
  is treated as not-external and inlined, so a CJS entry like
  `react/jsx-runtime` is evaluated as ESM: `module is not defined`, on every
  request.
- **`optimizeDeps.include`.** Resolved with no importer, walking up from `root`.
  A framework plugin names the dependencies the browser needs pre-bundled from
  CJS here.

An existing `node_modules` is never replaced. A real directory (a `pnpm install`)
or a link to a different directory stops the dev server with a message naming
it. Two importers' `node_modules` cannot both be at the workspace root, so two
dev servers using different `node_modules()` targets cannot run at once.

Add `node_modules` to `.gitignore` without a trailing slash: `node_modules/`
matches a directory, and this is a symlink.

A plugin, `bazel:npm-resolve`, stays behind it at `enforce: 'post'` as a
fallback: it locates `<node_modules>/<package>/package.json` and hands the id
back to the resolver anchored there, for an importer the walk cannot reach and
for a server that does no walk of its own. Exports maps, conditions and subpaths
stay the resolver's either way, so `import "zod/v4"` and a conditional `exports`
behave in dev as they do in a build.

A package the importer does not link produces Vite's `Failed to resolve import`
at the moment the browser asks for the module; add it to the `node_modules`
target's `deps`.

## Type Checking

The dev server does not type-check; neither does `vite dev`. Type errors come
from the editor and `bazel build`, and do not block the browser update. See
[IDE integration](../getting-started/ide-setup.md).

## CSS Modules

A `*.module.css` served by the dev server is scoped by Vite's own CSS modules
with Vite's defaults, as under `vite dev`. A `vite_config` reaches the dev
server through `plugins` alone, so a `css` key in one fails the config load
naming it, as [any other key does](#vite_config-what-it-may-import).

## vite-plugin-bazel

The `plugin` attribute wires `vite-plugin-bazel`, which:

- resolves generated code out of `bazel-bin`; without it every `ts_codegen`
  output is invisible to Vite;
- invalidates on a rebuild, so a codegen change arrives as an HMR update;
- makes the restart decision under
  [Watch Mode with ibazel](#watch-mode-with-ibazel).

`plugin = "@rules_typescript//vite:vite_plugin_bazel"` wires it; a
`ts_dev_server` without the attribute runs Vite alone.

## React Fast Refresh

`react_refresh = True` loads `@vitejs/plugin-react`, which preserves component
state across an HMR update. The package has to be in the `node_modules` tree:

```python
node_modules(
    name = "node_modules",
    deps = [
        "@npm//:vite",
        "@npm//:vitejs_plugin-react",
    ],
    hoist = "//:node_modules/.pnpm/node_modules",
)

ts_dev_server(
    name = "dev",
    server = "@rules_typescript//vite:dev_server",
    entry_point = ":app",
    node_modules = ":node_modules",
    react_refresh = True,
)
```

The entry point comes from the package's own `exports` map, so a `dist/`
reorganisation between majors does not move it. If the plugin cannot be loaded,
the dev server fails to start naming the target and the dep to add.

The attribute is Vite's: against a server whose `DevServerInfo` sets
`native_react_refresh`, `react_refresh = True` is an analysis-time error.

`@vitejs/plugin-react` finds its `react-refresh` runtime by the same walk-up Vite
uses for `rolldown`, so the target has to be [named `node_modules`](#setup).

## `vite_config`: What It May Import

`vite_config` takes one `.ts`, `.mts`, `.mjs` or `.js` file default-exporting
`{plugins: [...]}`, whose plugins are prepended to Bazel's. A framework's Vite
plugin runs in the dev server through it.

The rule loads a copy of the file in `bazel-bin`: Node resolves a runfiles
symlink before the file's own imports, so a source-tree config would resolve
them through a source-tree `node_modules`, which this ruleset does not have.
`//tests/dev_server:vite_config_boundary_test` pins the bare import and the
undeclared relative one;
`//tests/dev_server:dev_with_composed_user_config_behaviour_test` pins the
declared one:

- A bare npm specifier resolves through the tree the `node_modules` attr built.
  That target must be in the same Bazel package as the dev server, the directory
  Node finds walking up from the copy.
- A relative import resolves only if the module is declared in
  `vite_config_srcs`, which stages it beside the copy. An undeclared sibling is
  not there, and the dev server exits with
  `[rules_typescript] Failed to load vite_config: …` naming the file.

TypeScript works, as do the extensionless relative specifiers a
bundler-resolution config is written with, because the generated config loads the
file through Vite's own `loadConfigFromFile`.

The generated config reads `plugins` out of yours and nothing else: any other
key fails the load naming itself, since it would otherwise be silently
discarded. A framework config that sets `define`, `resolve.alias`,
`build.target` or `optimizeDeps` hits this, as does one carrying `root`, which
the dev server takes from the target. Move what you need into a plugin.

## Watch Mode with ibazel

```bash
go install github.com/bazelbuild/bazel-watcher/cmd/ibazel@latest
ibazel run //src/app:dev
```

`ibazel run` SIGTERMs the launcher after every rebuild and the launcher survives
it, so one Vite process lives across every rebuild. The restart-or-keep decision
is made inside that process:

| What changed | Handled by | Restarts Vite? |
|---|---|---|
| first-party source | Vite transform → HMR | no; Bazel is not involved |
| `ts_codegen` output | the plugin's `bazel-bin` watcher | no |
| the generated Vite config | `ConfigWatcher` | yes |
| the npm tree, or the Vite version in it | `ConfigWatcher` | yes, with a warning |
| the toolchain node binary | `ConfigWatcher` | yes, with a warning |

The generated config exports `bazelConfigInputs`: for each input, a path, its
content digest, and whether an in-process restart can fix a change to it. The
digest is over content, because Bazel rewrites outputs on every action and an
mtime says nothing. Only a new `bazel run` replaces a node binary or an npm tree,
hence the warning on the last two rows.

Vite restarts on a change to its own config file but has no concept of the thing
that generates it; `ConfigWatcher` watches those inputs.

Both watchers are `vite-plugin-bazel`'s, so a target without the `plugin` attr
keeps its server process across rebuilds and compares no digests.

## Edit-to-HMR Latency

The design goal is under 500 ms from save to browser update:

```
save ──▶ watcher notices ──▶ transform ──▶ HMR frame ──▶ browser re-executes
         └───────────────── measured ──────────────┘     └── not measured ──┘
```

`//tests/dev_server:{dev,dev_with_plugin}_hmr_latency_test` measures the
left-hand side: each starts its `ts_dev_server` as `bazel run` does, holds a
WebSocket open on the server's HMR endpoint as a browser would, saves a file,
and times the frame that comes back and the fetch that follows it.

```bash
# the numbers, on every run
bazel test //tests/dev_server:dev_hmr_latency_test --test_output=all --test_arg=-test.v

# a longer sample, for comparing a change against main
bazel test //tests/dev_server/... --test_env=HMR_ITERATIONS=100 \
    --test_output=all --test_arg=-test.v
```

The tests run in the ordinary suite. The only assertion is that the median stays
inside the whole 500 ms budget, some forty times the observed median. At that
ceiling what trips it is HMR falling back to a rebuild, a watcher gone to
polling, or the transform moving off the warm path.

Each run logs what it measured: min, median, p90 and max for both halves of the
loop, the cold first edit on its own, and which HMR message the server chose. The
fixture is two modules and the sample is twelve saves; `HMR_ITERATIONS` sets the
count. The two targets on one machine compare Vite with and without the plugin.

Vite treats an explicit `import.meta.hot.accept()` as a boundary and sends a
scoped update. The `what the server sent` line in the log says which message
arrived.

!!! note "Two saves inside 50 ms"
    Vite's watcher (chokidar) drops a second change to the same path within
    50 ms of the one it emitted; it is not deferred, it never arrives. A script
    that writes in a loop appears to hang. The benchmark spaces its samples for
    this reason.

## Attributes

| Attribute | Type | Default | Description |
|-----------|------|---------|-------------|
| `entry_point` | `label` | required | `ts_compile` target for the application entry point |
| `port` | `int` | `5173` | Dev server port |
| `host` | `string` | `"localhost"` | Dev server host. Set to `"0.0.0.0"` to bind on all interfaces |
| `open` | `bool` | `False` | Open the browser automatically on start. An analysis-time error against a server whose provider lists `server.open` in `ignored_config_fields` |
| `node_modules` | `label` | `None` | `node_modules` target providing the application's runtime deps, plus Vite on the Vite path; also what makes a bare npm import resolve; see [above](#how-a-bare-npm-specifier-resolves) |
| `plugin` | `label` | `None` | Compiled `vite-plugin-bazel` `.mjs`; see [above](#vite-plugin-bazel) |
| `server` | `label` | `@rules_typescript//oj:dev_server` | `DevServerInfo`-providing target choosing the implementation; see [above](#bringing-your-own-server) |
| `react_refresh` | `bool` | `False` | React Fast Refresh via `@vitejs/plugin-react`; requires `@npm//:vitejs_plugin-react` in the `node_modules` deps, and fails against a server applying Fast Refresh itself; see [above](#react-fast-refresh) |
| `vite_config_srcs` | `label_list` | `[]` | The local modules `vite_config` imports, staged beside it. A file outside the config's package is an analysis-time error |
| `vite_config` | `label` | `None` | A `.ts`/`.mts`/`.mjs`/`.js` file default-exporting `{plugins: [...]}`, prepended to Bazel's plugins; see [above](#vite_config-what-it-may-import) |

## Diagnostics

The launcher uses no host `node` or `vite`. A missing JS runtime toolchain fails
at analysis time; a missing `node_modules`, or a missing vite on the Vite path,
fails the launcher with a message. To see what it resolved:

```bash
bazel run //src/app:dev -- --dump-config
```

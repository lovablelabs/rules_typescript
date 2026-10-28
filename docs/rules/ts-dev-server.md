# ts_dev_server

Starts a dev server for a TypeScript application. The server transforms
first-party source in memory, so Bazel is out of the edit-to-browser loop;
`bazel-bin` supplies what it cannot produce itself (`ts_codegen` output, the
npm tree, assets, data srcs, passthrough `.d.ts`).

oj 0.2.49 is the default implementation at `@rules_typescript//oj:dev_server`.
Set `server = "@rules_typescript//vite:dev_server"` to use Vite. Other
implementations return `DevServerInfo`. See
[Bringing Your Own Server](../guides/dev-server.md#bringing-your-own-server).

The dev server does not type-check; type errors come from the editor and
`bazel build`.

Gazelle generates a `dev` target for a tsconfig program with non-test sources
and an application entry: a package index.html or a listed main.ts[x] or
app.ts[x]. Gazelle manages `entry_point`, `node_modules`, `plugin` and
`visibility`, and withdraws a `ts_dev_server` named `dev` (`dev_server` in a
package named `dev`) outside an application package, wherever it runs program
discovery, hand-written or not, unless a `# keep` above the rule holds it (a
`# keep` on `entry_point` alone keeps the rule but drops `node_modules`,
`plugin` and `visibility`). Gazelle keeps every other attribute as written
(`server`, `port`, `host`, `open`, `react_refresh`, `vite_config`,
`vite_config_srcs`) and every value under `# keep`.

oj provides native React Fast Refresh. Leave `react_refresh` false on the oj
path. Its serve root and cache location come from the launcher.

## Usage

This example selects Vite explicitly. Omit `server` to use oj.

```python
load("@rules_typescript//ts:defs.bzl", "ts_dev_server")
load("@rules_typescript//npm:defs.bzl", "node_modules")

node_modules(
    name = "dev_node_modules",
    deps = ["@npm//:vite"],
    hoist = "//:node_modules/.pnpm/node_modules",
)

# keep
ts_dev_server(
    name = "dev",
    server = "@rules_typescript//vite:dev_server",
    entry_point = ":app",
    node_modules = ":dev_node_modules",
    port = 5173,
    plugin = "@rules_typescript//vite:vite_plugin_bazel",
)
```

```bash
bazel run //src/app:dev
ibazel run //src/app:dev   # codegen rebuilds and config-aware restarts
```

The server runs from the package containing `ts_dev_server` and serves its
`index.html` at `/`. The `entry_point` may belong to another package. Bazel
output paths and npm resolution remain relative to the workspace.

## Attributes

| Attribute | Type | Default | Description |
|-----------|------|---------|-------------|
| `entry_point` | `label` | required | `ts_compile` target for the application entry point |
| `port` | `int` | `5173` | Dev server port |
| `host` | `string` | `"localhost"` | Dev server host. Set to `"0.0.0.0"` to bind on all interfaces |
| `open` | `bool` | `False` | Open the browser automatically on start. An analysis-time error against a server whose provider lists `server.open` in `ignored_config_fields` |
| `node_modules` | `label` | `None` | The importer's [`node_modules`](node-modules.md) target linking the application's runtime deps, plus Vite on the Vite path; also what makes a bare npm import resolve; see [npm Resolution](#npm-resolution) |
| `plugin` | `label` | `None` | Compiled `vite-plugin-bazel` `.mjs` file. It resolves generated code out of `bazel-bin`, invalidates on a rebuild, and makes the restart decision. Without it `bazel-bin` is invisible to Vite |
| `server` | `label` | `@rules_typescript//oj:dev_server` | `DevServerInfo`-providing target choosing the implementation; see [Dev Server](../guides/dev-server.md#bringing-your-own-server) |
| `react_refresh` | `bool` | `False` | React Fast Refresh via `@vitejs/plugin-react`, so component state survives an HMR update. Requires `@npm//:vitejs_plugin-react` in the `node_modules` deps; the dev server fails to start if the plugin cannot be loaded. An analysis-time error against a server whose provider sets `native_react_refresh` |
| `vite_config_srcs` | `label_list` | `[]` | The local modules `vite_config` imports, staged beside it so its relative imports resolve. A file outside the config's package is an analysis-time error |
| `vite_config` | `label` | `None` | A `.ts`/`.mts`/`.mjs`/`.js` file default-exporting `{plugins: [...]}`, prepended to Bazel's plugins. A framework's Vite plugin runs in the dev server this way. Loaded from a copy in `bazel-bin`, which bounds what it may import |

## npm Resolution

The common launcher exposes one app importer to every server implementation.
Vite adds the resolution fallback described below.

Starting the server links the importer's `node_modules` in at the workspace
root and removes the link on Ctrl-C, so a bare specifier resolves by the
ordinary walk up from the importer, and a package's own imports from its
realpath in the store. SSR externalisation and `optimizeDeps.include` resolve
without going through the plugin container, so they need the link. A generated
`bazel:npm-resolve` plugin at `enforce: 'post'` covers importers the walk cannot
reach. Exports maps, conditions and subpaths stay Vite's. For imports without a recorded compiler binding, a package the importer does not link produces Vite's `Failed to resolve import`; the fix is adding it to the `node_modules` target's `deps`.

`ts_dev_server` rejects a live executable source whose declared npm binding selects a different store File from the nearest link for that npm name on the dev app's complete `node_modules` importer chain. A missing link on that chain is accepted only for a binding from a workspace member's own importer outside that chain. The dev app stages that importer's links, and the generated `bazel:npm-resolve` plugin resolves the member's executable sources' bare imports of those names through them; anything else, such as a CSS `@import`, still resolves through the app's `node_modules`. Two member importers linking different stores for one name are rejected, since Vite pre-bundles a bare name once for every importer, and so is a member binding whose name the app chain links to a different store, since the walk up reaches the app's link first. Equal package versions do not establish store identity, while labels aliasing the same store File are accepted. The check runs before starting any selected server, including default oj, Vite and custom `DevServerInfo` providers. The error names the source, npm name and both owners. Align the stores or serve the sources in separate dev applications.

Source edits are served without rerunning Bazel, so a currently unused or type-only declared binding can become a runtime request, including a request for package metadata. Declared but unused or type-only differences, and differences a custom resolver could handle, can still be rejected. Ordinary same-store resolution stays with the selected server.

At Vite startup, the generated config checks declared source and asset paths for closer installations of their npm bindings. It rejects a different store directory, accepts links to the declared store, and preserves user files. This check runs when the config is evaluated: restart after changing an installation. It does not guarantee refusal in oj, which can continue after a plugin-host config error.

An existing `node_modules` is never replaced: a real directory or a link to
another one is an error naming both. In `.gitignore`, `node_modules` without a
trailing slash matches the symlink; `node_modules/` matches directories only.

## Workspace Members

A workspace member the application imports by package name reaches the
importer's `node_modules` as its store tree, a copy of the member's files
rather than a link to its source directory. The generated config names every
main-repository member in the `entry_point`'s npm closure in `ssr.noExternal`
and `optimizeDeps.exclude`, so the server transforms the member, its source
or the copy (below), like first-party source instead of externalising or
pre-bundling it. An excluded package is served as it is, so a member must ship
ES modules, and neither server discovers the dependencies a served copy
imports: a CommonJS one only a member imports goes in `optimizeDeps.include`
as `<member> > <dependency>` from a plugin's `config` hook. Published packages
keep the server's defaults, unless one shares a member's name: both lists match
by name.

The launcher sets `BAZEL_MEMBER_SOURCES` to a JSON object mapping each member's
store directory, as a real path, to the member's directory under the
workspace's real path, both with `/` separators. The store holds what the
member's compile publishes at the same relative paths: its sources for an
`emit = False` member, so a copied file maps to the source file itself, and its
emitted `.js` otherwise, which maps to where that `.js` would sit in the
checkout, beside sources that do not include it. Every server inherits the
launcher's environment, so a plugin reads it under any of them:

```js
const memberSources = Object.entries(JSON.parse(process.env.BAZEL_MEMBER_SOURCES ?? '{}'));

function sourcePath(file) {
  for (const [store, source] of memberSources) {
    if (file.startsWith(store + '/')) return source + file.slice(store.length);
  }
  return file;
}
```

When the application imports no member, the config sets neither key and the
launcher leaves `BAZEL_MEMBER_SOURCES` unset.

The generated config also resolves a module id under a member's store
directory to its counterpart in the member's directory: the same path, with a
`.js` copy found as `.ts`, `.tsx` or `.jsx` before `.js`, `.mjs` as `.mts` and
`.cjs` as `.cts`. Every plugin after it sees the source path, and an edit to
the source reaches the browser without a rebuild. A file with no counterpart
keeps its copy. A bare import in a member source resolves from the source
directory, except one only its copy reaches, such as a dependency the
application does not link, or one the source directory finds in another
package store than the copy does, such as the application's own version of a
dependency the member links at another version: both resolve as the copy's
would. A dependency pre-bundled under that name keeps its pre-bundle. Vite
offers this plugin every import. oj offers it bare imports only, before its
own resolver, and resolves a relative import from the importing file's
directory, so under oj a member source cannot import a file only its copy has,
such as generated output, and a copy-only file that imports the member's own
sources relatively loads them from the copy, a second instance beside the one
the application imports. A server-side render resolves a member the same way,
so it loads the source too, but an import it externalizes is resolved from the
source directory before this plugin runs, as Vite's SSR externalization does.

## `vite_config` Imports

Select the Vite server for Vite plugins and framework configuration.

The rule loads a copy of the file in `bazel-bin`, so its own imports resolve
beside the importer's `node_modules`. A bare npm specifier resolves through the
links of the `node_modules` target, provided that target is in the same Bazel
package as the dev server. A relative import resolves only if the module is declared in
`vite_config_srcs`; otherwise the server exits with
`[rules_typescript] Failed to load vite_config` naming the file.
`//tests/dev_server:vite_config_boundary_test` pins this; details in
[Dev Server](../guides/dev-server.md#vite_config-what-it-may-import).

## Restarts

The plugin-driven restart behavior below describes the Vite implementation.

Vite owns authored source edits. The Bazel watcher subscribes to generated and
published Files, using the selected File's `is_source` provenance. Handwritten
`declaredFiles` entries can set `isSource: true` to leave watching to Vite;
omitting it preserves their existing Bazel watcher behavior.

Generated TypeScript and JSON modules use their declared originals in
`bazel-bin`, even when checkout contains a file at the same workspace-relative
path. Their relative imports use source coordinates, and rebuilds update them
without requiring emitted JavaScript. For individually declared authored or
generated source Files, `ts_dev_server` records the nearest declared package
scope from the same repository. The Bazel plugin selects `#imports` targets
from that scope before filesystem lookup, maps first-party targets to their
declared Files, and delegates the resulting request to Vite.

Compiled `ts_codegen(out_dir)` trees retain the same declared authority over
their members. Imports select generated JavaScript instead of checkout twins,
and a missing member fails during module transformation instead of loading a
checkout copy. These trees contain compiled JavaScript and declarations, not
raw TypeScript sources.
Private imports in tree members and Files without a recorded scope use native
package resolution. Borrowing an outer authored scope for a tree member is
unsupported.

Vite resolves declared asset imports to their published Files in `bazel-bin`,
including relative references from published CSS and `?raw` and `?url` imports.
Source asset edits reach the server after their owning Bazel action rebuilds
those Files; the plugin watches the published outputs for updates.

Literal browser asset URLs such as `<img src="/logo.svg">` keep Vite's native
static lookup under the live application root. Declaring a Bazel File does not
remap those URLs. To use a declared generated image, import its URL with
`import logoUrl from "./logo.svg?url"` and use `logoUrl` as the image source.
Emitted asset URLs use Vite's native static serving and missing-file behavior.

One server process lives across every rebuild: `ibazel` SIGTERMs the launcher
and the launcher survives it. With `plugin` set, the restart decision is made
in that process, by comparing content digests of the inputs the generated
config was built from. A source edit and a `ts_codegen` rebuild do not restart
it; a change to the generated config, the npm tree or the toolchain node binary
does. See
[Dev Server](../guides/dev-server.md#watch-mode-with-ibazel).

## Edit-to-HMR Latency

For the Vite implementation, the budget is 500 ms from save to browser update.
`//tests/dev_server:{dev,dev_with_plugin}_hmr_latency_test` measures the
server's share of it by holding a WebSocket open as a browser would and saving a
file. The browser's own re-execution is outside the measurement. The suite
asserts that the median stays inside the budget and logs the distribution on
every run. See
[Dev Server](../guides/dev-server.md#edit-to-hmr-latency) for those logs and how
to run a longer sample.

## Diagnostics

```bash
bazel run //src/app:dev -- --dump-config
```

Prints the resolved launcher config (the selected server, runtime and
runfiles paths) and exits without starting the server.

See [Dev Server](../guides/dev-server.md) for the full guide.

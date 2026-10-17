# Tailwind v4

Tailwind v4 is a Vite plugin with no config file of its own, and goes in through
`vite_config` like any Vite plugin. Nothing in the rules is Tailwind-specific.

```typescript
// tailwind_config.ts
import tailwindcss from '@tailwindcss/vite';
import type { UserConfig } from 'vite';

const config: UserConfig = { plugins: [tailwindcss()] };

export default config;
```

```python
# keep
node_modules(
    name = "node_modules",
    deps = [
        "@npm_tailwind//:tailwindcss",
        "@npm_tailwind//:tailwindcss_vite",
        "@npm_tailwind//:vite",
    ],
    hoist = ":node_modules/.pnpm/node_modules",
)

# keep
ts_dev_server(
    name = "dev",
    server = "@rules_typescript//vite:dev_server",
    entry_point = ":entry",
    node_modules = ":node_modules",
    vite_config = "tailwind_config.ts",
)
```

All three packages have to be in the tree. `@tailwindcss/vite` reaches
`tailwindcss` and `@tailwindcss/oxide` by bare specifier from the staged config,
and `@tailwindcss/oxide` is a platform-specific native module: the lockfile
carries twelve platform builds of it beside the wrapper package, and the npm tree
places the one this host can load.

Both rules are hand-written, so each carries `# keep`. Without it Gazelle
rewrites the `node_modules` in a lockfile importer's package and withdraws it
elsewhere, and rewrites the dev server's `entry_point`, `node_modules`,
`plugin` and `visibility` in an application package and withdraws it
elsewhere; see [the dev server setup](dev-server.md#setup).

`//tests/tailwind` is the worked example: the dev server, started and
interrogated over HTTP.

## Class-Name Scanning

Tailwind v4 generates a rule only for a class name it finds. `@tailwindcss/vite`
scans from the dev-server package directory. For classes used in sibling
packages, add explicit `@source` paths relative to the stylesheet, such as
`@source "../shared";`. Files allowed by the dev server are not automatically
Tailwind scan inputs.

## `@import "tailwindcss"` and NODE_PATH

`@tailwindcss/node` resolves that import from the CSS file's own directory with
[enhanced-resolve](https://github.com/webpack/enhanced-resolve), which is not
Node's ESM resolver. In dev the stylesheet is served out of the source tree, with
no `node_modules` above it to walk up to.

enhanced-resolve reads `NODE_PATH`, and the dev server launcher prepends the npm
tree to it. Nothing in the config expresses this. Without it `bazel build` stays
green and the dev server answers 500 for the stylesheet, so the page renders
unstyled.

## Tailwind's npm Hub

Tailwind lives in `//tests/tailwind`'s own translated lockfile: twelve
platform-gated `oxide` packages would otherwise be resolved into a curated
fixture lockfile by a `pnpm add` that fixture cannot survive. A second hub costs
its own `ts_add_package` target and hand-written `deps` under `# keep`, since
Gazelle writes `@npm` alone. The stylesheet is a src of the `ts_compile` that
imports it. See [More than one hub](npm.md#more-than-one-hub).

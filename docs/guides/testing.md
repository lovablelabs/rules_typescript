# Testing with vitest

`ts_test` compiles TypeScript test files with `ts_compile`'s actions and runs
them inside the Bazel sandbox, under vitest by default. The attribute table and
every mechanism named here are in the [ts_test reference](../rules/ts-test.md);
this page is the recipes.

Tests written against node's own runner take
`runner = "@rules_typescript//ts/runners:node_test"`
([the node:test runner](../rules/ts-test.md#the-nodetest-runner)); the rest of
this page is the vitest runner.

## Setup

```python
# BUILD.bazel
load("@rules_typescript//ts:defs.bzl", "ts_compile", "ts_test")

ts_compile(
    name = "math",
    srcs = ["math.ts"],
    visibility = ["//visibility:private"],
)

ts_test(
    name = "math_test",
    srcs = ["math.test.ts"],
    deps = [":math", "@npm//:vitest"],
)
```

```bash
bazel test //path/to:math_test
```

The tests run in the importer chain tsgo checked them against: `node_modules`
names the nearest lockfile importer's target, each npm dep resolves to that
chain's link into the store, and every `ts_compile` dep's store files come
along, so the production code under test runs against the packages it
declared. `deps` lists what the tests
import, the npm imports of the package's production sources and the nearest
`package.json`'s dependencies;
`bazel run //:gazelle` writes that list from tsgo's listing of the package.
An import only some dep's own deps provide fails the build with the label to
add ([Deps have to be direct](../rules/ts-compile.md#deps-have-to-be-direct));
each package's own edges sit beside its store tree
([the layout](../rules/node-modules.md#the-layout)).

## A vitest Config

A config is always generated and always passed with `--config`. The `config`
file merges into it and every vitest setting is the file's, as under plain
`vitest`; see
[the generated vitest config](../rules/ts-test.md#the-generated-vitest-config).

```python
ts_test(
    name = "component_test",
    srcs = ["Button.test.tsx"],
    config = "vitest.config.ts",
    data = ["test/fixtures.json", "test/msw-handlers.ts"],
    deps = [
        ":button",
        "@npm//:react",
        "@npm//:happy-dom",
        "@npm//:testing-library_react",
        "@npm//:vitest",
    ],
)
```

```ts
// vitest.config.ts
export default {
  test: {
    environment: "happy-dom",
    setupFiles: ["./setupTests.ts"],
  },
};
```

`test.environment` takes any value vitest accepts (`node`, `jsdom`, `happy-dom`,
`edge-runtime`, or a custom environment package), and the matching package has
to be in `deps`; Gazelle writes it from the test program and the nearest
`package.json`. A file's `// @vitest-environment` docblock names that file's
environment over the config's, as under plain vitest; the compiled file keeps
the docblock ([Comments](../rules/ts-compile.md#comments)). Scoped npm names
take their label form: `@testing-library/react` is
`@npm//:testing-library_react`. `test.setupFiles` entries run before every
test file, which is where `matchMedia`, `ResizeObserver` and `PointerEvent`
belong; an entry naming a TypeScript source runs its compiled sibling, so the
`ts_compile` whose `srcs` hold `setupTests.ts` is in `deps` -- under Gazelle the
package's own compile, which is there already. `test.globalSetup` is the same
mechanism for a file that runs once around the whole run
([Setup Files](../rules/ts-test.md#setup-files)).

The modules the config imports relatively are `config_srcs`, each written at
its own path in the runfiles beside the config; Gazelle writes them from the
config's listing. A config that default-exports an array is read as a list of
vitest projects, each of which gets the Bazel layer too; the array becomes
`test.projects`, which needs vitest 3.2 or later
([A Config File](../rules/ts-test.md#a-config-file)).

Gazelle writes `config` from the file plain `vitest` would read -- a
`vitest.config.*`, else a `vite.config.*`, so a package that configures vitest
in its `vite.config.ts` runs under it -- beside the tests by name, else the one
in the nearest directory above holding a `package.json`, or the repository
root, as the label `//pkg:vitest_config` of a public `filegroup` it writes over
the file in that package. Vite's root is the config's package either way, so a
relative path in the config resolves against the directory it sits in;
`//tests/config_at_root` is the example. Gazelle writes the importers declaring
the config's npm packages into `config_node_modules`, which uses the same
runfiles staging as `data` while preserving the test's own npm resolution.
Existing authored `data` remains unchanged; first-party runtime owners of
config modules remain in `deps`
([what Gazelle writes](../gazelle/overview.md#what-gazelle-writes)).

## CSS Modules

A `*.module.css` in a `ts_compile`'s `srcs` is staged beside the compiled `.js`,
and vitest loads it as it does outside Bazel: the stylesheet is replaced by a
proxy whose properties are the class names `css.modules.classNameStrategy`
shapes, `_<name>_<hash>` under the default `stable` and the bare name under
`non-scoped`; Vite's CSS modules run on the file only under a `css` key in the
config. The Bazel layer sets no `css` key. The import is typed by the tsconfig
-- `vite/client` in `types`, or a `declare module "*.module.css"` in `srcs`.

## Coverage

```bash
bazel coverage //path/to:math_test
```

Works on every vitest `ts_test` when `@vitest/coverage-v8` is in `deps`. Which files are reported is `--instrumentation_filter`'s
answer, and `coverage_provider` picks between `"v8"` and `"istanbul"`; see
[ts_test § Coverage](../rules/ts-test.md#coverage).

## Cloudflare Workers

A Worker's tests can run inside workerd, so `SELF.fetch()` dispatches to the
`fetch` handler in the runtime. `@cloudflare/vitest-pool-workers` supplies the
pool; `//tests/workers_nested` is the worked example, in the shape a Worker
repository has: `package.json`, the vitest config, `wrangler.jsonc` and the
worker's tsconfig at the worker root, the tests in `test/`:

```jsonc
// workers/proxy/tsconfig.worker.json
{
  "compilerOptions": {
    "lib": ["esnext", "webworker"]
  }
}
```

```python
# workers/proxy/BUILD.bazel
load("@rules_typescript//ts:defs.bzl", "ts_compile", "ts_config")

package(default_visibility = ["//visibility:public"])

ts_config(
    name = "worker_tsconfig",
    src = "tsconfig.worker.json",
)

ts_compile(
    name = "worker",
    srcs = ["src/index.ts"],
    tsconfig = ":worker_tsconfig",
)

filegroup(
    name = "vitest_config",
    srcs = ["vitest.config.mjs"],
    visibility = ["//visibility:public"],
)

filegroup(
    name = "wrangler_config",
    srcs = ["wrangler.jsonc"],
    visibility = ["//visibility:public"],
)
```

```python
# workers/proxy/test/BUILD.bazel
ts_test(
    name = "worker_test",
    size = "medium",
    srcs = ["worker.test.ts"],
    config = "//workers/proxy:vitest_config",
    coverage_provider = "istanbul",
    tsconfig = "//workers/proxy:worker_tsconfig",
    wrangler_config = "//workers/proxy:wrangler_config",
    deps = [
        "//workers/proxy:worker",
        "@npm_workers//:cloudflare_vitest-pool-workers",
        "@npm_workers//:vitest",
        "@npm_workers//:vitest_coverage-istanbul",
    ],
)
```

Gazelle writes the two filegroups from the config -- `vitest_config` over the
file, `wrangler_config` over the file its `configPath` names -- and the test's
`config`, `wrangler_config`, `coverage_provider` and `deps` from the config's
imports ([a Workers-pool config](../gazelle/overview.md#a-workers-pool-config)).

```typescript
/// <reference types="@cloudflare/vitest-pool-workers/types" />
import { SELF } from 'cloudflare:test';
import { describe, expect, it } from 'vitest';

describe('worker', () => {
  it('answers /health', async () => {
    const res = await SELF.fetch('https://example.com/health');
    expect(res.status).toBe(200);
  });
});
```

`tsconfig` names the worker's file on the worker target and on the test target:
the `Request`/`Response` globals a Worker is written against are in `webworker`,
which no set `target` implies, and the tests are checked under their own
tsconfig's `lib`; the same file on both makes the worker's sources and the
tests one program ([the test's
program](../rules/ts-test.md#the-tests-program)). The
`ts_config` puts the file behind a label the test's package can name; a test in
the worker's own package names the file directly, `tsconfig =
"tsconfig.worker.json"`, as `//tests/workers` does
([the test's tsconfig](../rules/ts-test.md#the-tests-tsconfig)).

### The vitest Config

```javascript
import { cloudflareTest } from '@cloudflare/vitest-pool-workers';

export default {
  plugins: [
    cloudflareTest({
      wrangler: { configPath: './wrangler.jsonc', environment: 'test' },
    }),
  ],
};
```

`cloudflareTest()` belongs in `plugins`, not in `test.pool`. Two of the
package's exports are candidates. `cloudflarePool()` is a pool initializer that
boots workerd and nothing else. `cloudflareTest()` is a Vite plugin that
installs that pool and owns the `cloudflare:test` specifier: `resolveId` maps it
to a virtual id, `load` returns the runtime's bytes. The pool forwards
`cloudflare:test` to Vite and externalises every other `cloudflare:*` specifier
to workerd, so with no plugin registered nothing resolves it and vitest falls
back to Node package resolution, which fails.

`configPath` is relative to the config file, and `ts_test` roots Vite at the
config's package, so it names the file it names under plain `vitest`. That
file's `main` is `src/index.ts`, the deploy entry; `wrangler_config` stages a
copy whose `main` and `env.test.main` are `src/index.js`, the compiled worker,
at the file's own path, and that is the config the pool reads. A `rules` module
the worker imports (`import greeting from "./greeting.txt"`) is a src of the
`ts_compile`, which puts it in the runfiles. What else a wrangler config can
name is in [A Workers Pool](../rules/ts-test.md#a-workers-pool).
`//tests/workers` is the
same-package shape: the config beside the tests, `main: "src/index.js"`, and
the file in `data`.

### `coverage_provider` and `cloudflare:test`

`coverage_provider = "istanbul"`: v8 coverage is counters read back out of
Node's inspector, and workerd has none; istanbul instruments before the code
crosses into the runtime, so `bazel coverage` reports per-line data for code
running inside workerd.

The pool's ambient declaration for `cloudflare:test` is the `exports` subpath
`@cloudflare/vitest-pool-workers/types`, whose only condition is `types`.
Nothing imports it: the test file names it in a `/// <reference types>`
directive, as above, or the test's tsconfig names it in `types`, and tsgo
resolves either through the importer's link to the pool package, which `deps`
names
([a `types` entry that names a package](../rules/ts-compile.md#a-types-entry-that-names-a-package)).

## Snapshots

`toMatchSnapshot()` works, and the `.snap` files stay where a plain `vitest` run
keeps them: `<package>/__snapshots__/<source>.snap`, beside the `.ts` and not in
`bazel-out`. The `.snap` is a src of the test, as every other file under the
package is, which is what puts it inside the sandbox; Gazelle lists it with the
package's files:

```python
ts_test(
    name = "widget_test",
    srcs = [
        "__snapshots__/widget.test.ts.snap",
        "widget.test.ts",
    ],
    deps = [":widget", "@npm//:vitest"],
)
```

`ts_test` runs vitest in read-only snapshot mode, so a snapshot the test cannot
read is a failure; in vitest's default mode an unlisted snapshot would be
written into the sandbox as new, and the test would pass on what it had just
written. Writing one is vitest's own `vitest -u`, run in the package as outside
Bazel. Commit the result.

## Sharding

Set `shard_count` on the target ([Sharding](../rules/ts-test.md#sharding)).

## Watch Mode

Use [ibazel](https://github.com/bazelbuild/bazel-watcher) to re-run tests on
every change:

```bash
go install github.com/bazelbuild/bazel-watcher/cmd/ibazel@latest

ibazel test //path/to:my_test
ibazel test //...
```

ibazel watches the build graph, so only affected targets are rebuilt and
re-tested. To see what the launcher resolved (node binary, vitest entry, the
chain's `node_modules`):

```bash
bazel run //path/to:my_test -- --dump-config
```

## Build Feedback

```bash
bazel test //... --show_result=10   # default is 1
bazel test //... --show_result=0    # all targets
```

Add `test --show_result=20` to `.bazelrc` to make it permanent.

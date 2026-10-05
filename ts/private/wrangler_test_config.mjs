import { chmodSync, copyFileSync, existsSync, realpathSync } from "node:fs";
import { createRequire } from "node:module";
import { join, posix, resolve } from "node:path";

const fail = (message) => {
  process.stderr.write(`wrangler_test_config: ${message}\n`);
  process.exit(1);
};

const names = {
  "--config": "config",
  "--out": "out",
  "--config-path": "configPath",
  "--node-modules": "nodeModules",
  "--runtime-file": "runtimeFiles",
  "--runtime-source": "runtimeSources",
  "--runtime-js": "runtimeJs",
};
const flags = {
  nodeModules: [],
  runtimeFiles: [],
  runtimeSources: [],
  runtimeJs: [],
};
const argv = process.argv.slice(2);
for (let i = 0; i < argv.length; i += 2) {
  const name = names[argv[i]] ?? argv[i];
  if (Array.isArray(flags[name])) flags[name].push(argv[i + 1]);
  else flags[name] = argv[i + 1];
}
const { config, out, configPath, nodeModules } = flags;
if (!config || !out || !configPath || nodeModules.length === 0) {
  fail("--config, --out, --config-path and --node-modules are all required");
}

// Resolved from the pool package's realpath in the store, the walk the pool's
// `import("wrangler")` makes, so the copy is patched by the reader that parses it.
const pool = nodeModules
  .map((dir) => join(resolve(dir), "@cloudflare", "vitest-pool-workers"))
  .find((dir) => existsSync(dir));
if (!pool) {
  fail(
    `@cloudflare/vitest-pool-workers is linked by none of ${nodeModules.join(", ")}. Did you mean to set workers_pool to the config's pool owner or add its package to the test's deps?`,
  );
}
const require_ = createRequire(join(realpathSync(pool), "_anchor.cjs"));
let wrangler;
try {
  wrangler = require_("wrangler");
} catch (error) {
  fail(`wrangler is not beside the pool's tree: ${error.message}`);
}
const { experimental_readRawConfig, experimental_patchConfig } = wrangler;

const runtimeFiles = new Map();
for (const pair of flags.runtimeFiles) {
  const [source, runtime] = JSON.parse(pair);
  const selected = runtimeFiles.get(source) ?? new Set();
  selected.add(runtime);
  runtimeFiles.set(source, selected);
}
const paired = new Set([...runtimeFiles.values()].flatMap((runtimes) => [...runtimes]));
const runtimeSources = new Set(flags.runtimeSources.filter((path) => !paired.has(path)));
const runtimeJs = new Set(flags.runtimeJs.filter((path) => !paired.has(path)));
const compiledExtensions = { ts: "js", tsx: "js", mts: "mjs", cts: "cjs" };
const runtimeEntry = (main) => {
  const source = posix.normalize(posix.join(posix.dirname(configPath), main));
  let selected = runtimeFiles.get(source);
  if (!selected) {
    const emitted = source.replace(
      /\.(ts|tsx|mts|cts)$/,
      (_, ext) => `.${compiledExtensions[ext]}`,
    );
    selected = new Set();
    if (runtimeSources.has(source)) selected.add(source);
    if (runtimeJs.has(emitted)) selected.add(emitted);
    if (selected.size === 0) return main;
  }
  if (selected.size !== 1) {
    fail(
      `${configPath}: ${main} has conflicting declared runtime owners; use one runtime identity in the test dependencies`,
    );
  }
  const [runtime] = selected;
  return runtime === source
    ? main
    : posix.relative(posix.dirname(configPath), runtime);
};

copyFileSync(config, out);
// A sandboxed or cached input is read-only, and copyFile keeps that mode.
chmodSync(out, 0o644);
const { rawConfig } = experimental_readRawConfig({ config: out });
const patch = {};
if (typeof rawConfig.main === "string") patch.main = runtimeEntry(rawConfig.main);
for (const [name, env] of Object.entries(rawConfig.env ?? {})) {
  if (env && typeof env.main === "string")
    (patch.env ??= {})[name] = { main: runtimeEntry(env.main) };
}
if (Object.keys(patch).length === 0)
  fail(`${config} names no \`main\`, so the pool has no worker to boot`);
try {
  experimental_patchConfig(out, patch);
} catch (error) {
  fail(`cannot patch ${config}: ${error.message}`);
}

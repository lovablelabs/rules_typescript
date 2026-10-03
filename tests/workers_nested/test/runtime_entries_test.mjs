import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { spawnSync } from "node:child_process";

const [patcher, nodeModules] = process.argv.slice(2);
const temp = mkdtempSync(join(process.env.TEST_TMPDIR, "entries-"));
writeFileSync(join(temp, "entry.ts"), "export const incidental = true;\n");
let count = 0;
function project(main, runtimeFiles, env, runtimeSources = [], runtimeJs = [], readOnly = false) {
  const config = join(temp, `config-${count++}.json`);
  const out = config + ".out.json";
  const original = JSON.stringify({ name: "runtime-entry", main, env });
  writeFileSync(config, original);
  if (readOnly) chmodSync(config, 0o444);
  const result = spawnSync(
    process.execPath,
    [
      patcher,
      "--config",
      config,
      "--config-path",
      "../fixture/worker/wrangler.json",
      "--out",
      out,
      "--node-modules",
      resolve(nodeModules),
      ...runtimeFiles.flatMap((pair) => ["--runtime-file", JSON.stringify(pair)]),
      ...runtimeSources.flatMap((path) => ["--runtime-source", path]),
      ...runtimeJs.flatMap((path) => ["--runtime-js", path]),
    ],
    { encoding: "utf8" },
  );
  assert.equal(readFileSync(config, "utf8"), original);
  return {
    ...result,
    config: result.status === 0 ? JSON.parse(readFileSync(out, "utf8")) : undefined,
    writable: result.status === 0 && (statSync(out).mode & 0o200) !== 0,
  };
}
// Bazel stages generated and cached inputs read-only, and copying keeps that mode;
// the mode bit is asserted because a root executor would write the copy anyway.
const readOnly = project(
  "./entry.ts",
  [["../fixture/worker/entry.ts", "../fixture/worker/entry.js"]],
  undefined,
  [],
  [],
  true,
);
assert.equal(readOnly.config?.main, "entry.js");
assert.ok(readOnly.writable, "the patched copy of a read-only config stays read-only");
for (const [source, emitted] of [
  ["ts", "js"],
  ["tsx", "js"],
  ["mts", "mjs"],
  ["cts", "cjs"],
]) {
  const main = `./src/../entry.${source}`;
  const src = `../fixture/worker/entry.${source}`;
  const js = `../fixture/worker/entry.${emitted}`;
  assert.equal(project(main, [[src, src]]).config?.main, main);
  assert.equal(project(main, [[src, js]]).config?.main, `entry.${emitted}`);
  const movedJs = `../fixture/worker/worker/entry.${emitted}`;
  assert.equal(
    project(main, [[src, movedJs]]).config?.main,
    `worker/entry.${emitted}`,
  );
  const movedSource = `../fixture/worker/worker/entry.${source}`;
  assert.equal(
    project(main, [[src, movedSource]]).config?.main,
    `worker/entry.${source}`,
  );
  const ambiguous = project(main, [
    [src, movedSource],
    [src, movedJs],
  ]);
  assert.notEqual(ambiguous.status, 0);
  assert.match(ambiguous.stderr, /conflicting declared runtime owners/);
}
for (const extension of ["js", "mjs", "cjs"]) {
  const main = `entry.${extension}`;
  const runtime = `../fixture/worker/${main}`;
  assert.equal(project(main, [[runtime, runtime]]).config?.main, main);
}
const environment = project(
  "entry.ts",
  [
    ["../fixture/worker/entry.ts", "../fixture/worker/worker/entry.js"],
    ["../fixture/worker/env.ts", "../fixture/worker/worker/env.ts"],
  ],
  {
    test: { main: "./env.ts", vars: { GREETING: "from-env-test" } },
    unavailable: { main: "missing.ts" },
  },
);
assert.equal(environment.config?.main, "worker/entry.js");
assert.equal(environment.config?.env.test.main, "worker/env.ts");
assert.equal(environment.config?.env.test.vars.GREETING, "from-env-test");
assert.equal(environment.config?.env.unavailable.main, "missing.ts");
const legacySource = "../fixture/worker/entry.ts";
const legacyJs = "../fixture/worker/entry.js";
const legacyEmitted = project(
  "entry.ts",
  [],
  { test: { main: "./entry.ts" } },
  [],
  [legacyJs],
);
assert.equal(legacyEmitted.config?.main, "entry.js");
assert.equal(legacyEmitted.config?.env.test.main, "entry.js");
assert.equal(
  project("entry.ts", [], undefined, [legacySource]).config?.main,
  "entry.ts",
);
const legacyAmbiguous = project(
  "entry.ts",
  [],
  undefined,
  [legacySource],
  [legacyJs],
);
assert.notEqual(legacyAmbiguous.status, 0);
assert.match(legacyAmbiguous.stderr, /conflicting declared runtime owners/);
assert.equal(
  project("entry.ts", [[legacySource, legacySource]], undefined, [], [legacyJs])
    .config?.main,
  "entry.ts",
);
assert.equal(
  project(
    "entry.ts",
    [[legacySource, "../fixture/worker/moved/entry.js"]],
    undefined,
    [legacySource],
    [legacyJs],
  ).config?.main,
  "moved/entry.js",
);
assert.equal(
  project("missing.ts", [], undefined, [], [legacyJs]).config?.main,
  "missing.ts",
);
const opaque = "../fixture/worker/entry.js";
assert.equal(project("entry.ts", [[opaque, opaque]]).config?.main, "entry.ts");
const tree = "../fixture/worker/generated";
assert.equal(
  project("generated/entry.ts", [[tree, tree]]).config?.main,
  "generated/entry.ts",
);
assert.equal(project("missing.ts", []).config?.main, "missing.ts");
console.log(
  "Exact runtime pairs project main and env.main; conflicts reject and unmatched identities remain unchanged.",
);

const assert = require("node:assert/strict");
const { execFileSync } = require("node:child_process");
const { readFileSync } = require("node:fs");
const { test } = require("node:test");
const state = require("./runfiles_state.cjs");
const culori = require("culori");
const [modulePath, packagePath, dataPath, mappedModulePath] = JSON.parse(
  execFileSync(process.env.RUNFILES_LOOKUP_HELPER, JSON.parse(process.env.RUNFILES_LOOKUP_KEYS), { encoding: "utf8" }),
);

test("runfiles lookup retains repository mapping", () => {
  assert.strictEqual(mappedModulePath, modulePath);
});

test("runfiles lookup retains declared data bytes", () => {
  assert.deepStrictEqual(readFileSync(dataPath), readFileSync(process.env.RUNFILES_ORIGINAL_DATA));
});

test("runfiles lookup cannot create a second declared module instance", () => {
  assert.ok(require(modulePath) === state, "declared module lookup created a second instance");
});

test("runfiles lookup cannot create a second npm store instance", () => {
  assert.ok(require(packagePath) === culori, "npm store lookup created a second instance");
});

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { version } from "typescript";

import { bump, dir } from "./helper";

test("runs as CommonJS at its runtime view path", () => {
  assert.equal(typeof require, "function");
  assert.equal(__dirname, join(process.cwd(), "_main/tests/node_test/cjs"));
});

test("an extensionless relative require keeps the runfiles path", () => {
  assert.equal(bump(1), 2);
  assert.equal(dir, __dirname);
});

test("a data entry is beside __dirname", () => {
  const note = readFileSync(join(__dirname, "fixtures/note.txt"), "utf8");
  assert.equal(note.trim(), "beside the CommonJS test");
});

test("a named import from a CommonJS dependency is its export", () => {
  assert.match(version, /^\d+\.\d+\./);
});

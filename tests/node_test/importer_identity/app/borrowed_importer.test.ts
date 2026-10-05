import assert from "node:assert/strict";
import { test } from "node:test";
import { assertImporterIdentity } from "./identity.js";

test("staging preserves borrowed importer resolution and unrelated data symlinks", () => {
  assertImporterIdentity();
  assert.match(import.meta.resolve("minimatch"), /\/minimatch@9\.0\.9\/node_modules\/minimatch\//);
});

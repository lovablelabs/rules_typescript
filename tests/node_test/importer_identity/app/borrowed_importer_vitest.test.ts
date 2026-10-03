import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { test } from "vitest";
import { assertImporterIdentity } from "./identity.js";

test("staging preserves borrowed importer resolution and unrelated data symlinks", () => {
  assertImporterIdentity();
  assert.equal(createRequire(import.meta.url)("minimatch/package.json").version, "9.0.9");
});

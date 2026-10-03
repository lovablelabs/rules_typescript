import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

test("runs at its runtime view path", () => {
  assert.equal(
    fileURLToPath(import.meta.url),
    join(process.cwd(), "_main/tests/node_test/runfiles_layout.test.js"),
  );
});

test("has its own source staged beside it", () => {
  assert.ok(existsSync(new URL("./runfiles_layout.test.ts", import.meta.url)));
});

test("has a data entry beside it", () => {
  const url = new URL("./fixtures/note.txt", import.meta.url);
  const note = readFileSync(url, "utf8");
  assert.equal(note.trim(), "the data entry was beside the test");
});

test("reads an opaque data tree whose name ends in .json", () => {
  const url = new URL("./fixtures.json/note.txt", import.meta.url);
  assert.equal(readFileSync(url, "utf8").trim(), "the data entry was beside the test");
});

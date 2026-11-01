import assert from "node:assert/strict";
import { realpathSync } from "node:fs";
import { createRequire } from "node:module";
import test from "node:test";
import { fileURLToPath } from "node:url";

import stripAnsi from "strip-ansi";

const ESC = String.fromCharCode(27);

test("strip-ansi resolves ansi-regex beside its own tree", () => {
  assert.equal(stripAnsi(`${ESC}[4mplain${ESC}[0m`), "plain");
  const entry = realpathSync(fileURLToPath(import.meta.resolve("strip-ansi")));
  const ansiRegex = createRequire(entry).resolve("ansi-regex");
  const beside = /\/\.pnpm\/ansi-regex@6\.5\.0\/node_modules\/ansi-regex\//;
  assert.match(ansiRegex, beside);
});

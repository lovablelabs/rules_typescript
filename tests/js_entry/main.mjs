import assert from "node:assert/strict";
import { existsSync, lstatSync, readFileSync } from "node:fs";
import { writeFile } from "node:fs/promises";
import { greet } from "./greet.mjs";

const args = process.argv.slice(2);
const out = args[0] === "--out" ? args[1] : "";

if (args[0] === "--check-data-links") {
  for (const extension of ["js", "json"]) {
    const live = new URL(`./binary_data_links/valid.${extension}`, import.meta.url);
    const dangling = new URL(`./binary_data_links/dangling.${extension}`, import.meta.url);
    assert(lstatSync(live).isSymbolicLink());
    assert.equal(readFileSync(live, "utf8"), "linked data\n");
    assert(lstatSync(dangling).isSymbolicLink());
    assert.throws(() => readFileSync(dangling), { code: "ENOENT" });
  }
}

if (!out) {
  process.stdout.write(greet("runfiles") + "\n");
} else {
  const nodeModules = process.env.TS_CODEGEN_NODE_MODULES ?? "";
  await writeFile(
    out,
    [
      `export const greeting: string = ${JSON.stringify(greet("codegen"))};`,
      `export const nodeBinary: boolean = ${Boolean(process.env.NODE_BINARY)};`,
      `export const nodeModulesDir: boolean = ${Boolean(nodeModules) && existsSync(nodeModules)};`,
      "",
    ].join("\n"),
  );
}

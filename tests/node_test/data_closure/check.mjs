import assert from "node:assert/strict";
import { readFileSync, realpathSync } from "node:fs";
import { createRequire } from "node:module";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

export function checkDataSibling() {
  const helper = join(process.env.RUNFILES_DIR, "_main/tests/node_test/data_closure/helper.cjs");
  assert.equal(createRequire(import.meta.url)(helper), "data module reached its source sibling");
}

export async function checkDataClosure() {
  const root = join(process.env.RUNFILES_DIR, "_main/tests/node_test/data_closure");
  for (const [file, content] of [
    ["source_data.ts", 'export { sourceValue } from "./source_leaf.js";\n'],
    ["source_leaf.ts", 'export const sourceValue: string = "source-only fixture";\n'],
  ]) {
    assert.equal(readFileSync(join(root, file), "utf8"), content);
  }
  assert.equal(createRequire(import.meta.url)("minimatch/package.json").version, "10.2.4");
  const dependency = join(root, "foreign/dependency.js");
  const dependencyAlias = join(root, "adapter/foreign/dependency.js");
  assert.equal(realpathSync(dependencyAlias), realpathSync(dependency));
  assert.equal(
    await import(pathToFileURL(dependencyAlias).href),
    await import(pathToFileURL(dependency).href),
  );
  const helper = pathToFileURL(join(root, "adapter/foreign/helper.js")).href;
  const { observe } = await import(helper);
  assert.deepEqual(observe(), {
    version: "9.0.9",
    expanded: true,
    note: "transitive data reached the emitted dependency",
  });
}

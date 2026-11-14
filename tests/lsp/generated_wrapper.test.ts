import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { transformWithOxc } from "vite";

// The runfiles hold the fixture's authored configs and no .bazel/ tree, as a checkout has before its first refresh.
const fixture = join(process.cwd(), "_main/tests/integration/lsp/generated");

test("vite loads the generated-mode tsconfig.json before refresh installs the editor projects", async () => {
  assert.ok(existsSync(join(fixture, "tsconfig.json")), "the wrapper is missing from the runfiles");
  const consumer = join(fixture, "consumer.ts");
  await transformWithOxc(readFileSync(consumer, "utf8"), consumer);
});

test("vite rejects a wrapper that references a missing .bazel project directly", async () => {
  const dir = mkdtempSync(join(process.env.TEST_TMPDIR || tmpdir(), "single-level-"));
  writeFileSync(
    join(dir, "tsconfig.json"),
    JSON.stringify({ files: [], include: [], references: [{ path: "./.bazel/tsconfig/generated_test.json" }] }),
  );
  const consumer = join(dir, "consumer.ts");
  writeFileSync(consumer, "export const value: number = 1;\n");
  await assert.rejects(transformWithOxc(readFileSync(consumer, "utf8"), consumer), /TSCONFIG_ERROR.*generated_test\.json/);
});

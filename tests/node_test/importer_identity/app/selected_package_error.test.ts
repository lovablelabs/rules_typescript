import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

for (const [entry, manifest] of [
  ["export", { exports: "./missing.mjs" }],
  ["legacy main", { main: "./missing.cjs" }],
] as const) {
  test(`a selected package's missing ${entry} cannot fall back to another package`, () => {
    for (const location of ["native", "fallback"]) {
      const root = mkdtempSync(path.join(process.env.TEST_TMPDIR ?? tmpdir(), "package-entry-"));
      try {
        const consumer = path.join(root, "consumer", "node_modules");
        const borrowed = path.join(root, "borrowed");
        const fallback = path.join(root, "fallback", "node_modules");
        const selectedPackage = path.join(
          location === "native" ? path.join(borrowed, "node_modules") : consumer,
          "broken-entry",
        );
        const fallbackPackage = path.join(fallback, "broken-entry");
        mkdirSync(borrowed, { recursive: true });
        mkdirSync(selectedPackage, { recursive: true });
        mkdirSync(fallbackPackage, { recursive: true });
        writeFileSync(path.join(selectedPackage, "package.json"), JSON.stringify(manifest));
        writeFileSync(
          path.join(fallbackPackage, "package.json"),
          JSON.stringify({ exports: "./index.mjs" }),
        );
        writeFileSync(path.join(fallbackPackage, "index.mjs"), "export default 'wrong package';\n");
        const hookIndex = process.execArgv.indexOf("--import");
        assert.notEqual(hookIndex, -1);
        const hook = process.execArgv[hookIndex + 1];
        assert.ok(hook?.endsWith("node_test_hook.mjs"));
        const run = () =>
          spawnSync(
            process.execPath,
            [
              "--import",
              hook,
              "--input-type=module",
              "--eval",
              'console.log((await import("broken-entry")).default)',
            ],
            {
              cwd: borrowed,
              env: { ...process.env, NODE_PATH: [consumer, fallback].join(path.delimiter) },
              encoding: "utf8",
            },
          );
        const child = run();
        assert.ifError(child.error);
        assert.notEqual(child.status, 0, child.stderr);
        assert.match(child.stderr, /ERR_MODULE_NOT_FOUND/);
        writeFileSync(
          path.join(selectedPackage, "package.json"),
          JSON.stringify({ exports: "./index.mjs" }),
        );
        writeFileSync(
          path.join(selectedPackage, "index.mjs"),
          "export default 'selected package';\n",
        );
        const repaired = run();
        assert.ifError(repaired.error);
        assert.equal(repaired.status, 0, repaired.stderr);
        assert.equal(repaired.stdout.trim(), "selected package");
      } finally {
        rmSync(root, { recursive: true, force: true });
      }
    }
  });
}

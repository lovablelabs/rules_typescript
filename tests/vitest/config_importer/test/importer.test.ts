import { createRequire } from "node:module";
import { minimatch } from "minimatch";
import { expect, it } from "vitest";

declare const __CONFIG_ANSWER__: number;
declare const __CONFIG_MATCH__: boolean;
declare const __CONFIG_MINIMATCH_VERSION__: string;

const require = createRequire(import.meta.url);

it("loads the sibling config through its own npm importer", () => {
  expect(__CONFIG_ANSWER__).toBe(42);
  expect(__CONFIG_MATCH__).toBe(true);
  expect(__CONFIG_MINIMATCH_VERSION__).toBe("10.2.4");
});

it("keeps the test's npm resolution at the test importer", () => {
  expect(minimatch("src/answer.ts", "**/*.ts")).toBe(true);
  expect(minimatch("src/answer.js", "**/*.ts")).toBe(false);
  expect(require("minimatch/package.json").version).toBe("9.0.9");
  expect(() => require.resolve("zod")).toThrow(/Cannot find module/);
});

import { createRequire } from "node:module";
import { expect, it } from "vitest";

declare const __CONFIG_HOISTED_VALUE__: number;
declare const __CONFIG_HOISTED_VERSION__: string;

it("retains a config-only member's hidden hoist without exposing its imports to the test", () => {
  expect(__CONFIG_HOISTED_VALUE__).toBe(42);
  expect(__CONFIG_HOISTED_VERSION__).toBe("5.0.1");
  expect(() => createRequire(import.meta.url).resolve("config-hoist-member")).toThrow(/Cannot find module/);
  expect(() => createRequire(import.meta.url).resolve("zod")).toThrow(/Cannot find module/);
});

import { execFileSync } from "node:child_process";
import { isAbsolute } from "node:path";
import { expect, it } from "vitest";

it("spawns the runtime tool through its environment variable", () => {
  const tool = process.env.RT_PROBE_TOOL ?? "";
  expect(isAbsolute(tool)).toBe(true);
  const out = execFileSync(tool, ["from", "ts_test"], { encoding: "utf8" });
  expect(out.trim()).toBe("probe ran with from ts_test");
});

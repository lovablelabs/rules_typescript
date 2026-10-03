import { describe, expect, it } from "vitest";
import answer from "virtual:answer";

describe("user config merges with the generated config", () => {
  it("resolves a virtual module from the user config's plugin", () => {
    expect(answer).toBe(42);
  });

  it("is found through the include the generated config writes", () => {
    expect(expect.getState().testPath).toMatch(
      /\/_merge_test\.vitest\/tests\/_main\/tests\/vitest\/config_merge\/merge\.test\.ts$/,
    );
    expect(import.meta.url).toMatch(/\/merge\.test\.js$/);
  });
});

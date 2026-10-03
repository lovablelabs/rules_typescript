import { describe, expect, it } from "vitest";

describe("vitest workspace project", () => {
  it("runs inside a named project through the merged include", () => {
    expect(expect.getState().testPath).toMatch(
      /\/_project_test\.vitest\/tests\/_main\/tests\/vitest\/workspace\/project\.test\.ts$/,
    );
    expect(import.meta.url).toMatch(/\/project\.test\.js$/);
  });
});

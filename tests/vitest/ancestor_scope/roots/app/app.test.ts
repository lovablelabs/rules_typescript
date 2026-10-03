import { expect, test } from "vitest";

import { answer } from "../support/helper";

test("a relocated module runs under its ancestor package scope", () => {
  expect(answer).toBe(42);
});

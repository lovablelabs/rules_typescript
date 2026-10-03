import { expect, test } from "vitest";
import value from "./value.cjs";

test("copied JavaScript retains its runtime dependency", () => {
  expect(value).toBe(42);
});

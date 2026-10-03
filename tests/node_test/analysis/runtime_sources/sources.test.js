import { expect, test } from "vitest";
import { value } from "./source_value.js";

test("copied JavaScript retains a source dependency", () => {
  expect(value).toBe(42);
});

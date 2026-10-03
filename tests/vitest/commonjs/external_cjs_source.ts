import { expect, it } from "vitest";

import { answer } from "./helper";

it("retains an external compiler's ES twin for a borrowed test source", () => {
  expect(answer()).toBe(42);
});

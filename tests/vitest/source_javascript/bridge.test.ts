import { expect, it } from "vitest";

import { answer } from "./bridge.js";

it("reads the dependency's JavaScript beside its current TypeScript input", () => {
  expect(answer).toBe(43);
});

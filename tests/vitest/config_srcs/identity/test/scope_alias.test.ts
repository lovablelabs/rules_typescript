import { expect, it } from "vitest";

import { value } from "../components/value.js";

declare const __CONFIG_SCOPE__: { name: string; type: string };

it("config staging preserves an emitted dependency's unchanged ancestor scope", () => {
  expect(value).toBe(37);
  expect(__CONFIG_SCOPE__).toEqual({
    name: "scope-identity-fixture",
    type: "module",
  });
});

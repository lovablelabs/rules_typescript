import { expect, test } from "vitest";

import { engineUrl, step } from "../lib/engine";
import { stateUrl } from "../lib/state";

const lib = "/_main/tests/vitest/cross_package/test/lib/";

test("a src of another package runs as its compiled module", () => {
  expect(step().count).toBe(1);
  expect(engineUrl.endsWith(`${lib}engine.js`)).toBe(true);
  expect(stateUrl.endsWith(`${lib}state.js`)).toBe(true);
});

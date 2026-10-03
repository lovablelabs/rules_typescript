import { expect, test } from "vitest";

import { leafIdentities } from "../imports/helper";

test("a moved module's package imports reach the published leaf", async () => {
  const [emitted, source] = await leafIdentities();
  expect(source).toBe(emitted);
});

import { expect, test } from "vitest";

import { marker } from "../kept/marker";
import { leaf } from "../owned/leaf";

test("a module moved under another scope's directory keeps its own scope", () => {
  expect(leaf + marker).toBe(3);
});

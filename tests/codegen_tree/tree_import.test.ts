import vitestManifest from "vitest/package.json?url";
import { expect, test } from "vitest";

import { greeting } from "#codegen/tree/messages/greeting";

test("a file inside a directory output loads by extensionless import, and an npm asset loads by URL", () => {
  expect(greeting(2)).toBe("greeting: 2");
  expect(vitestManifest).toMatch(/package\.json$/);
});

import { test } from "vitest";
import { checkDataClosure, checkDataSibling } from "./check.mjs";

test("data modules retain sibling imports, producer npm and transitive runtime closure", async () => {
  checkDataSibling();
  await checkDataClosure();
});

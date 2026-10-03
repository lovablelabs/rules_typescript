import { readFileSync } from "node:fs";

export const { answer } = JSON.parse(
  readFileSync(new URL("./data-payload.json", import.meta.url), "utf8"),
);

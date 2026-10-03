import { readFileSync } from "node:fs";

export function readNote() {
  return readFileSync(new URL("./note.txt", import.meta.url), "utf8").trim();
}

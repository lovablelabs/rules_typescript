import { minimatch } from "minimatch";
import { createRequire } from "node:module";
import { readNote } from "./dependency.js";

export function observe() {
  return {
    version: createRequire(import.meta.url)("minimatch/package.json").version,
    expanded: minimatch("module.ts", "*.{js,ts}"),
    note: readNote(),
  };
}

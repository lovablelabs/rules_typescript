import { readFileSync } from "node:fs";
export { cloudflareTest } from "@cloudflare/vitest-pool-workers";

export const poolVersion = JSON.parse(readFileSync(
  new URL("./node_modules/@cloudflare/vitest-pool-workers/package.json", import.meta.url),
  "utf8",
)).version;

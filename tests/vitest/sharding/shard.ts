import { readdirSync } from "node:fs";
import { basename } from "node:path";
import { fileURLToPath } from "node:url";
import { expect } from "vitest";

const FILES = ["alpha", "bravo", "charlie", "delta", "echo", "foxtrot"];

export function shardFiles(index: number, total: number): string[] {
  return FILES.filter((_, i) => i % total === index);
}

export function expectOwnShard(url: string): void {
  const total = Number(process.env.TEST_TOTAL_SHARDS);
  const index = Number(process.env.TEST_SHARD_INDEX);
  expect(total).toBe(3);
  const own = shardFiles(index, total);
  expect(own.map((name) => `${name}.test.js`)).toContain(basename(fileURLToPath(url)));
  const root = process.env.TS_TEST_FILES_ROOT ?? "";
  const staged = readdirSync(root, { recursive: true, encoding: "utf8" })
    .filter((p) => p.endsWith(".test.ts"))
    .map((p) => basename(p))
    .sort();
  expect(staged).toEqual(own.map((name) => `${name}.test.ts`));
}

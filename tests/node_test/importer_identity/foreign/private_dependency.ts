import { minimatch } from "minimatch";

export function privateMatch(): boolean {
  return minimatch("private.ts", "*.ts");
}

export function privateResolution(): { esm: string; parent: string } {
  return { esm: import.meta.resolve("minimatch"), parent: import.meta.url };
}

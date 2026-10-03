import assert from "node:assert/strict";
import { lstatSync, readFileSync, readlinkSync } from "node:fs";
import { createRequire } from "node:module";
import { minimatch } from "minimatch";
import { borrowedValue } from "../foreign/borrowed.js";
import { privateMatch, privateResolution } from "../foreign/private_dependency.js";

export function assertImporterIdentity() {
  assert.equal(minimatch("consumer.ts", "*.ts"), true);
  assert.equal(borrowedValue(), "borrowed source + nested source");
  assert.equal(privateMatch(), true);
  const privateModule = privateResolution();
  const privateRequire = createRequire(privateModule.parent);
  const privatePackage = /\/minimatch@10\.2\.4\/node_modules\/minimatch\//;
  assert.match(privateModule.esm, privatePackage);
  assert.match(privateRequire.resolve("minimatch"), privatePackage);
  assert.equal(privateRequire("minimatch/package.json").version, "10.2.4");
  const consumerRequire = createRequire(import.meta.url);
  assert.equal(consumerRequire("minimatch/package.json").version, "9.0.9");
  for (const extension of ["json", "js"]) {
    for (const [name, target] of [
      ["valid", "payload.txt"],
      ["dangling", "absent.txt"],
    ]) {
      const link = new URL(`./fixture_links/${name}.${extension}`, import.meta.url);
      assert.equal(lstatSync(link).isSymbolicLink(), true);
      assert.equal(readlinkSync(link), target);
      if (name === "valid") assert.equal(readFileSync(link, "utf8"), "linked data\n");
    }
  }
}

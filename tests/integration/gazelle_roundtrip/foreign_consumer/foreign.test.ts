import { expect, it } from "vitest";
import { answer, generatedLocales } from "../foreign_json/index.js";

it("retains another package library's transitive TypeScript, JavaScript and JSON paths", () => {
  expect(answer).toBe(42);
});

it("reads the generated package.json through the library dependency", () => {
  expect(generatedLocales).toEqual(["en", "sv"]);
});

import {describe, expect, it} from "vitest";

import {add} from "@math";
import {origin as fallback} from "@fallback";
import {origin as point} from "@point";
import {type Point, origin, valueUrl} from "@shared/value";

describe("tsconfig paths", () => {
  it("a pattern alias resolves to the compiled module", () => {
    const p: Point = {x: 1, y: 2};
    expect(p.x + p.y).toBe(3);
    expect(origin).toEqual({x: 0, y: 0});
    expect(valueUrl).toMatch(/\/shared\/value\.js$/);
  });

  it("an alias naming a .ts file is the same compiled module", () => {
    expect(point).toBe(origin);
  });

  it("an alias skips a candidate no file answers, as tsc does", () => {
    expect(fallback).toBe(origin);
  });

  it("an alias naming another package's .ts runs its compiled dep", () => {
    expect(add(2, 3)).toBe(5);
  });
});

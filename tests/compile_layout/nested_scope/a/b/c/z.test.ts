import { z } from "./z.js";

if (z !== "z") {
  throw new Error(`unexpected ${z}`);
}

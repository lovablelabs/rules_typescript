import MagicString from "magic-string";
import { nestedValue } from "./nested/borrowed.js";

export function borrowedValue() {
  return new MagicString("borrowed source").append(` + ${nestedValue()}`).toString();
}

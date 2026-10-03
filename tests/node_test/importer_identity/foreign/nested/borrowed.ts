import MagicString from "magic-string";

export function nestedValue() {
  return new MagicString("nested source").toString();
}

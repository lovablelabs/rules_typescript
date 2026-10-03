import { expect, it } from "vitest";

declare const __CONFIG_POOL_VERSION__: string;

it("boots the sibling config's pool without a pool in the test importer", () => {
  expect(navigator.userAgent).toBe("Cloudflare-Workers");
  expect(__CONFIG_POOL_VERSION__).toBe("0.22.0");
});

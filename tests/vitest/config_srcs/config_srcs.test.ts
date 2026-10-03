import { lstatSync, readFileSync, readlinkSync, realpathSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, it } from "vitest";

declare const __CONFIG_SRCS__: string;
declare const __CONFIG_HELPER_URL__: string;
declare const __CONFIG_SCOPE__: {
  exports: { ".": string };
  imports: { "#value": string };
};

it("config modules keep authored identities and data symlinks", () => {
  expect(__CONFIG_SRCS__).toBe("staged beside the config");
  expect(__CONFIG_SCOPE__.exports["."]).toBe("./config_srcs.test.ts");
  expect(__CONFIG_SCOPE__.imports["#value"]).toBe("./config_srcs.test.ts");
  const helper = fileURLToPath(__CONFIG_HELPER_URL__);
  expect(helper.endsWith("/tests/vitest/config_srcs/plugins/define.ts")).toBe(true);
  expect(realpathSync(helper)).toBe(helper);
  expect(lstatSync(helper).isFile()).toBe(true);
  for (const extension of ["json", "js"]) {
    for (const [name, target] of [
      ["valid", "payload.txt"],
      ["dangling", "absent.txt"],
    ]) {
      const link = new URL(`./fixture_links/${name}.${extension}`, import.meta.url);
      expect(lstatSync(link).isSymbolicLink()).toBe(true);
      expect(readlinkSync(link)).toBe(target);
      if (name === "valid") expect(readFileSync(link, "utf8")).toBe("linked data\n");
    }
  }
});

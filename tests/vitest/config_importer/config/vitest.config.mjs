import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { minimatch } from "minimatch";
import { z } from "zod";

const require = createRequire(import.meta.url);

export default {
  root: fileURLToPath(new URL("../", import.meta.url)),
  define: {
    __CONFIG_ANSWER__: JSON.stringify(z.number().parse(42)),
    __CONFIG_MATCH__: JSON.stringify(minimatch("config/value.ts", "**/*.ts")),
    __CONFIG_MINIMATCH_VERSION__: JSON.stringify(require("minimatch/package.json").version),
  },
};

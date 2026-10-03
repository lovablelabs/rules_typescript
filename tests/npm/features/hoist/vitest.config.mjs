import { createRequire } from "node:module";
import { z } from "config-hoist-member";

const require = createRequire(import.meta.url);
const fromMember = createRequire(require.resolve("config-hoist-member"));
const fromZod = createRequire(fromMember.resolve("zod"));

export default {
  define: {
    __CONFIG_HOISTED_VALUE__: JSON.stringify(z.number().parse(42)),
    __CONFIG_HOISTED_VERSION__: JSON.stringify(fromZod("ansi-regex/package.json").version),
  },
};

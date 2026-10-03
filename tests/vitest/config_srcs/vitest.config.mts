import { defineConfig } from "vitest/config";

import scope from "./package.json";
import { defineFromMeta } from "./plugins/define";

export default defineConfig({
  plugins: [defineFromMeta()],
  define: { __CONFIG_SCOPE__: JSON.stringify(scope) },
});

import { defineConfig } from "vitest/config";

import scope from "../package.json";

export default defineConfig({
  define: { __CONFIG_SCOPE__: JSON.stringify(scope) },
});

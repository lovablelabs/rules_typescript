import { cloudflareTest, poolVersion } from "./pool/setup.mjs";

export default {
  define: { __CONFIG_POOL_VERSION__: JSON.stringify(poolVersion) },
  plugins: [cloudflareTest({ wrangler: { configPath: "./wrangler.jsonc" } })],
};

import { mergeConfig } from "vitest/config";

import meta from "./meta.json";

export const defineFromMeta = () => ({
  name: "config-srcs-define",
  config: () =>
    mergeConfig(
      {},
      {
        define: {
          __CONFIG_SRCS__: JSON.stringify(meta.value),
          __CONFIG_HELPER_URL__: JSON.stringify(import.meta.url),
        },
      },
    ),
});

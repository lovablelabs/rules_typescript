### Added

- **`ts_dev_server(cloudflare = True)` for an app whose `vite_config` loads
  `@cloudflare/vite-plugin`.** oj's TanStack Start dev server defers the client
  bundle until it listens only when the `--config` file's text names the
  plugin, and the generated config reached it through an import alone, so a
  Cloudflare app bundled its whole client before binding. With the attribute
  the generated config names the plugin and logs plugin output, the Worker's
  console and errors among it, to stdout/stderr, which oj had silenced.
  Gazelle sets it when the target's `vite_config`, or a `vite.config.*` it
  names, names the plugin.

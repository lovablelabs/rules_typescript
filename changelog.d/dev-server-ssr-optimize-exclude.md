### Fixed

- **`ts_dev_server` keeps workspace members out of every environment's dep
  optimizer.** The generated config listed members only in the top-level
  `optimizeDeps.exclude`, which Vite applies to the client environment alone.
  When a framework or worker plugin turned on dep discovery for the `ssr` or a
  worker environment, that environment pre-bundled a member, and a pre-bundle
  never reaches plugin transforms such as StyleX. A `configEnvironment` hook
  now adds the same exclude list to every non-client environment; a
  consumer's own exclude list still merges in. A member a plugin lists in an
  environment's `optimizeDeps.include` is still pre-bundled there.

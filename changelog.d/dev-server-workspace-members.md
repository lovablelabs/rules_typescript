### Added

- **`ts_dev_server` treats the workspace members an application imports as
  first-party source and tells plugins where they live.** A member reaches
  `node_modules` as a copy of its files, so a plugin that skips
  `node_modules` skips it. The generated config names each member in
  `ssr.noExternal` and `optimizeDeps.exclude`; the launcher sets
  `BAZEL_MEMBER_SOURCES`, a JSON map from each member's store directory to
  its directory in the workspace, so the plugin can match the copy by its
  path there. docs/rules/ts-dev-server.md § Workspace Members.

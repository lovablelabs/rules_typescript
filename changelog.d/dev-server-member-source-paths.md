### Changed

- **`ts_dev_server` serves a workspace member from its source files.** The
  generated config resolves a member's copy in `node_modules` to its
  counterpart at the same path in the member's directory, so plugins that skip
  `node_modules` transform it and source edits reach the browser without a
  rebuild, in a server-side render too. A bare import the source directory
  finds in another package store than the copy's resolves as the copy's, so
  the member keeps its own version of a dependency. Under oj a member source
  cannot import a file only its copy has. oj 0.2.44 carries a patch, until an
  oj release ships it, that offers bare imports to `pre` resolveId plugins
  before its own resolver, in the browser and in SSR, and answers a plugin's
  `this.resolve` with its own resolver after the `pre` plugins and before the
  others, the pre-bundle for a pre-bundled import, as Vite does.
  docs/rules/ts-dev-server.md § Workspace Members.

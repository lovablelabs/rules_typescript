### Fixed

- **Under oj, a server-side render resolves an import with a query through
  plugins, query included.** oj's Start host stripped `?url`, `?raw`,
  `?inline` and `?worker` before plugin resolveId, so
  `@lovablelabs/fru/styles.css?url` loaded fru's stylesheet instead of its URL
  module, and that plugin-loaded `.css` module was compiled as TSX and failed
  the render. A dependency import with a query is no longer handed to Node.
  oj 0.2.49 compiles a plugin-loaded sheet as CSS, and 0.2.50 ships the
  query passthrough and the SSR externalization rule.

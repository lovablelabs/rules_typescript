### Fixed

- **Under oj, a server-side render resolves an import with a query through
  plugins, query included.** oj 0.2.44's Start host stripped `?url`, `?raw`,
  `?inline` and `?worker` before plugin resolveId, so
  `@lovablelabs/fru/styles.css?url` loaded fru's stylesheet instead of its URL
  module, and that plugin-loaded `.css` module was compiled as TSX and failed
  the render. A dependency import with a query is no longer handed to Node.
  Carried as `oj/oj-start-host-query-resolve-id.patch` and
  `oj/oj-server-ssr-css-url.patch` until an oj release ships them.

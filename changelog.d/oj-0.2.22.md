### Changed

- **The default oj dev server moves from 0.2.16 to 0.2.22.** oj now carries
  the `fs.watch` dispatch fix itself (0.2.17: matching runs off the notify
  thread and each event visits only the watchers indexed on its path, an
  ancestor or its file identity), and an edit to a file a plugin's `resolveId`
  answers for hot-updates, with module-graph nodes carrying the served URL
  (0.2.19). Dependency pre-bundling now runs only on rolldown (the app's
  Vite 8 or its installed `rolldown`; deps are served per file when there is
  none), with discovery on by default: oj crawls the app's entries and
  pre-bundles every dependency they import, and `optimizeDeps.noDiscovery`
  turns that off. A pre-bundled chunk imports an entry under the entry's
  versioned URL, so the entry evaluates once (0.2.22). Start dev serves client
  chunks with ETags, `immutable` caching and gzip, and holds the browser
  reload until the editor's flush when a rebuild is in flight; stopping the
  dev server closes it, so plugins dispose what they started, before
  plugin-spawned children such as workerd are reaped; a `resolveId` into a
  symlinked package is no longer a 403; and an edit reached through a symlink
  updates the module Vite keyed by its real path. The V8 code cache keys
  Vite's per-load `.timestamp-<ms>-<hash>.mjs` config bundles by a stable
  name, removes caches of other V8 versions, and trims itself at startup to
  `OJ_CODE_CACHE_MAX_BYTES` (default 128 MiB, `0` for no bound) (0.2.20).
  The V8, deno_core and Cranelift pins are unchanged. Three oj patches are
  dropped because oj ships their changes: `fs.watch` dispatch (0.2.17),
  plugin-resolved file HMR (0.2.19) and the code cache bound (0.2.20).
  `oj/oj-server-scan-outside-plugin-host.patch` stays: on a large
  Cloudflare/TanStack Start app the time-sliced in-host scan still runs past
  the optimizer's 120 s deadline, and the deadline stops the plugin host's
  JavaScript mid-hook, so SSR requests in flight fail. The scan runs in the
  optimizer job with rolldown's own resolver instead.

### Changed

- **The default oj dev server moves from 0.2.22 to 0.2.44.** Elapsed time no
  longer fails plugin work: config extraction, plugin hooks, plugin host init
  and the dependency pre-bundle run unbounded, as in Vite, and
  `OJ_PLUGIN_TIMEOUT`, `OJ_PLUGIN_INIT_TIMEOUT`, `OJ_EXTRACT_TIMEOUT`,
  `OJ_OPTIMIZE_TIMEOUT` and `OJ_PRESEED_TIMEOUT` are gone (0.2.25).
  TanStack Start dev applies edits in place through React Fast Refresh
  (0.2.37), honors `src/start.ts` (0.2.26), CSS modules and preprocessors
  (0.2.30, 0.2.31) and the app's `resolve.alias` in client bundles (0.2.33).
  Dev servers honor `server.https` (0.2.27); redirects behind plugin
  middleware and `server.proxy` reach the browser with their cookies (0.2.42);
  and the watcher recovers events lost to an inotify overflow (0.2.44).
  `GET /@oj/diagnostics` and `OJ_LOG_JSON=1` report structured diagnostics
  (0.2.23). The V8, deno_core and Cranelift pins are unchanged. Three oj
  patches are dropped: oj ships the parked-call watchdog fix and the
  restart's launch environment (0.2.33), and the in-host dependency scan
  that `oj/oj-server-scan-outside-plugin-host.patch` kept out of the plugin
  host no longer runs under a deadline that stops in-flight hooks (0.2.25,
  0.2.33). `oj/oj-cache-start-bundle-stable-key.patch` is re-cut: oj still
  drops its own build dir from the Start bundle key by path prefix, which
  misses under a symlinked `bazel-bin`.

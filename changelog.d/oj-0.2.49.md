### Changed

- **The default oj dev server moves from 0.2.22 to 0.2.49, built from oj's
  git tag `v0.2.49`.** `oj/Cargo.lock` pins every oj crate to that tag's
  commit, so Bazel fetches one checkout at a fixed sha. Fetching it needs
  `git` on PATH and access to github.com; a crates.io mirror no longer
  covers it, and the pin is the commit id, not a content digest. Elapsed
  time no longer fails plugin work: config extraction, plugin hooks,
  plugin host init and the dependency pre-bundle run unbounded, as in
  Vite, and
  `OJ_PLUGIN_TIMEOUT`, `OJ_PLUGIN_INIT_TIMEOUT`, `OJ_EXTRACT_TIMEOUT`,
  `OJ_OPTIMIZE_TIMEOUT` and `OJ_PRESEED_TIMEOUT` are gone (0.2.25).
  TanStack Start dev applies edits in place through React Fast Refresh
  (0.2.37), honors `src/start.ts` (0.2.26), CSS modules and preprocessors
  (0.2.30, 0.2.31) and the app's `resolve.alias` in client bundles (0.2.33).
  Dev servers honor `server.https` (0.2.27); redirects behind plugin
  middleware and `server.proxy` reach the browser with their cookies (0.2.42);
  and the watcher recovers events lost to an inotify overflow (0.2.44).
  TanStack Start bundles and the dependency pre-bundle run on the rolldown
  built into oj, plugin hook filters gate like Vite's, and
  `@vitejs/plugin-react` runs natively (0.2.45). StyleX compiles natively,
  `OJ_OTEL=1` exports OpenTelemetry, and a non-root `base` routes as in Vite
  (0.2.46). TanStack Start dev runs one plugin host for every environment
  and evaluates `vite.config` once (0.2.47). Debug output moves to
  `oj dev --debug[=scope]` (`DEBUG=oj:<scope>`), replacing `OJ_BOOT_PHASES`,
  `OJ_DEBUG_MEM` and `OJ_DEBUG_HOOK_GATE`; Start links stylesheets in
  Vite's order; and FSEvents streams are shared as chokidar shares them
  (0.2.48). oj returns the memory the npm rolldown binding frees (0.2.49).
  `GET /@oj/diagnostics` and `OJ_LOG_JSON=1` report structured diagnostics
  (0.2.23). The V8, deno_core, Cranelift and tokio pins are unchanged;
  `notify` gives way to oj's `oj_notify` fork. Three oj
  patches are dropped: oj ships the parked-call watchdog fix and the
  restart's launch environment (0.2.33), and the in-host dependency scan
  that `oj/oj-server-scan-outside-plugin-host.patch` kept out of the plugin
  host no longer runs under a deadline that stops in-flight hooks (0.2.25,
  0.2.33). `oj/oj-cache-start-bundle-stable-key.patch` is re-cut: oj still
  drops its own build dir from the Start bundle key by path prefix, which
  misses under a symlinked `bazel-bin`. `oj/oj-server-pre-resolve-id.patch`
  is re-cut onto 0.2.49's single plugin host, whose ssr environment is a
  container of the dev host and which passes `scan` on itself, and
  `oj/oj-server-ssr-css-url.patch` drops its plugin-loaded stylesheet hunk,
  which 0.2.46 ships.

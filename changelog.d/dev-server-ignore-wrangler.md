### Fixed

- **`ts_dev_server`'s file watcher ignores `.wrangler` directories.** wrangler
  and miniflare keep their local state under `.wrangler/` and rewrite its
  SQLite files on every request a Cloudflare worker serves in dev, and each
  write reached the watcher as a source change. The generated config now sets
  `server.watch.ignored` to `**/.wrangler/**`. A `vite_config` cannot set this
  itself, because the generated config takes only its `plugins`.

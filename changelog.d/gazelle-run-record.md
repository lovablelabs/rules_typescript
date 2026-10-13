### Changed

- **A TypeScript-only Gazelle run whose inputs are all unchanged since a run
  that changed nothing exits at once.** With `-ts_listing_cache`, a full
  recursive `fix` or `diff` run of `gazelle_typescript` keeps a run record
  beside the listings, after gazelle-rs's own: every walked directory's
  entries, the BUILD files by content, the listings' reads, the files the
  extension opened and the binary, keyed by the arguments, with the run's
  exit status and log. On a large app's workspace an unchanged tree went from
  about 14 s to about 1 s, most of it Bazel's own. A replay writes the
  recorded log back. Any change falls back to the full pass, and only a full
  pass that exits 0, writes no BUILD file and saw no input change near or
  during it is recorded. `-ts_run_record=verify` checks a replay against a
  full pass without caches, its exit status and log included, and
  `-ts_run_record=off` disables it.

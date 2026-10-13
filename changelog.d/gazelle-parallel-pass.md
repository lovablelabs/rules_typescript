### Changed

- **A warm full Gazelle run decodes its cached listings ahead of the walk.**
  With `-ts_listing_cache`, a full recursive run decodes the cached listings on
  a worker pool as it starts, after gazelle-rs's early decode, instead of one
  at a time as the walk reaches each `tsconfig.json`. It decodes the listings
  the last full run used, recorded in the cache directory as
  `listings-taken`, so entries no run reads any more are left alone. A
  program's `srcs` also find their producing rules through one index per
  BUILD file instead of a scan per src. On a large app's workspace, against
  a Gazelle that already replays Resolve from its resolve record, an
  unchanged tree with `-ts_run_record=off` went from about 7.6 s to about
  7.3 s, and a BUILD-only edit from about 8.0 s to about 7.7 s.
  `-ts_parallel=verify` reads each listing again when the walk reaches it and
  exits on any difference; `-ts_parallel=off` decodes as before. Generated
  BUILD files are unchanged.

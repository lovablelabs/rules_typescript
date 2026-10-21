### Added

- **`-ts_memo=record` keeps what each Resolve task read, asked and did, and
  `-ts_memo=verify` checks the last pass's copy.** With `-ts_listing_cache` a
  full pass stores, per completion and first-pass edge resolution, digests of
  its inputs, the rules, outputs and BUILD files it read, the answers the
  lockfile, manifests, rule index, walk state and package configurations gave
  it with their arguments, and its outputs in order. A verify pass compares
  each task whose stored reads still hold with its stored outputs, fails on a
  difference, and tallies clean and stale tasks with their time. Nothing
  replays from the store yet. The default, `off`, is unchanged; under
  `-ts_readsets=verify` a speculation now stops at the first lookup of a
  vitest config's wrangler config, as it does at other first-only logs.

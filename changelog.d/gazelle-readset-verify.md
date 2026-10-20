### Added

- **`-ts_readsets=verify` checks that Resolve's program completions and
  first-pass edge resolutions can be precomputed.** Each is speculated with its
  rule writes, logs and fatals held back and its reads recorded; the serial
  pass then fails when a speculation whose reads still hold differs, or when a
  rule changed without a recorded write. A final line tallies the tasks that
  agreed, conflicted or could not be speculated. First-pass speculation needs
  Gazelle 0.54's compat update loop. The default, `off`, is unchanged.

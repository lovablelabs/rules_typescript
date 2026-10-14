### Fixed

- **Under Gazelle 0.54, `gazelle_typescript` finishes its rules before Gazelle
  writes them.** Gazelle 0.54's update loop writes every BUILD file before it
  calls `AfterResolvingDeps`, so a resolve-record replay wrote rules Gazelle
  never resolved (bare `ts_compile`, `ts_config` and `filegroup` stubs, the
  root package included), and a full pass dropped what that phase adds, such
  as a new `emit = True` or `package_scopes`, and kept the scope candidates it
  removes. Under that Gazelle the first `Resolve` now resolves, merges and
  finishes every generated rule, or replays the record, so `-mode=fix`,
  `diff` and `print` all see the finished files. `-ts_resolve_record=verify`
  now replays the record and checks it against a full pass with the record
  off, so it sees what the replay wrote rather than what it held in memory.
  `e2e/gazelle_054` runs most of the extension's tests under Gazelle 0.54.

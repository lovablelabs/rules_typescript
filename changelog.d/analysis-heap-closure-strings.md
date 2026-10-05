### Fixed

- **Analysis no longer retains closure-sized strings and per-file view
  artifacts for every program.** Each `ts_compile` and `ts_test` program
  rendered a compiler overlay for every dependency declaration at analysis and
  retained the strings for as long as the configured target stayed cached. A
  `node` or `node_test` native view declared a transport copy or link per
  runtime File and a view output per runfiles entry. The overlays are now
  rendered at execution through `map_each`. A view input reached at one path
  is materialized there with no transport copy, an alias resolves straight to
  its target, and each view directory with no link beneath it is one tree
  artifact; links stay declared symlinks, which remote execution preserves and
  a tree artifact would not. The launcher config is written without
  indentation. 50 `node_test` targets over a 3,000-module closure retain
  0.44 MB each after analysis, down from 1.44 MB.

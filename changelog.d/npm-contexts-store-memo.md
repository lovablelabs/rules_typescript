### Fixed

- **A vitest `ts_test` with a large npm closure no longer spends minutes
  resolving npm stores before vitest starts.** The launcher resolved every
  store path once per (context, binding) pair: 5.4 million `EvalSymlinks` and
  `Stat` calls over 241 distinct stores for one 16,000-module test, about
  150 s per test action on remote execution. It now resolves each store once
  and skips a lookup whose directory and bindings set it has already placed.
  The links it creates are unchanged. A conflict between two bindings sets in
  one directory now names the first context that claimed the destination
  instead of the last.

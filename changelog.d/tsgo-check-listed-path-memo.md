### Fixed

- **A locally sandboxed `TsgoCheck` of a large program no longer spends
  minutes mapping its listing to declared inputs.** In a local sandbox tsgo
  lists npm files at their real paths, and each such edge was matched against
  every declared npm store with one `EvalSymlinks` per store: 12.5 million
  calls for the 30,237 npm edges of a large app's program over 412 stores.
  Each listed path is now resolved once per action, and a store whose file is
  not the listed one is ruled out with a single `stat`. The resolved edges and
  diagnostics are unchanged. `TsgoCheck`, `TsEmit`, the lint and the editor
  actions also leave their program root for a sandbox or remote executor to
  discard; in Bazel's own exec root they still remove it.

### Changed

- **A vitest `ts_test` with many modules loads its generated config faster.**
  The config built its module lookups by walking every module's ancestor
  directories with `path.resolve` and `path.relative`, twice per module: for a
  large app's test with 16,859 modules, 0.83 s of the config's 1.08 s load,
  on every test action and shard. It now walks each distinct (source
  directory, runtime directory) pair once, and the load takes 0.40 s. The
  lookups are unchanged.

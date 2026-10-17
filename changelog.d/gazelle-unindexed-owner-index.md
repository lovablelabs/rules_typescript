### Changed

- **Gazelle no longer scans every stored rule when an import has no indexed
  owner.** Resolving such an import, and every package scope a compiler
  config reads, walked all of the run's stored rules, about 5 ms a call on a
  large repository, in both the first resolve pass and the closure pass. Only
  a stored `ts_compile`, or the resolving rule itself, can hold the import, so
  the run now lists those once per change to the stored rules and checks only
  them. Output is unchanged.

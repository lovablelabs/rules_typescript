### Changed

- **Gazelle's unresolved-import notes and edge fatals no longer depend on walk
  order.** A program's `imports X: ...; no dep` notes print sorted by the
  imported file, each naming its smallest importer instead of the first one the
  walk reached. An edge fatal (an emit expression, a source `ts_compile` cannot
  accept, a foreign declaration in a workspace member, a discovery conflict)
  no longer exits at once: Resolve, or the completion of programs, finishes
  first, then the run exits with the smallest offending (program, file,
  import). The exit status is unchanged.

### Changed

- **Gazelle's resolve pass refreshes a package's program rules with a third
  less CPU.** The refresh computes the package's sources once and hands them to
  both the regenerated rules and each rule's compiler program lookup, instead
  of walking the package listing again per rule. On a synthetic tree of 300
  packages of 30 files, refresh CPU drops from 0.79 s to 0.53 s of a warm full
  pass. Output is unchanged.

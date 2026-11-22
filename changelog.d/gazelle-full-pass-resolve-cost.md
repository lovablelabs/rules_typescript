### Changed

- **Gazelle's two rule indexes rebuild from a list of the stored rules.**
  Resolve keeps an index of the stored `ts_compile` owners and one of the
  stored proto libraries and wrappers, and rebuilds each the first time it is
  read after a rule store: 331 rebuilds over 12,568 rules on a large
  workspace's full pass. Each rebuild parsed every rule's key and looked its
  package's configuration up again, 10 of the 58 seconds the pass's main
  goroutine ran with every listing cached. The emission graph now keeps its
  keys in a list beside the map, each with its parsed label, and a rebuild
  walks the list and keeps what it derives from a key alone: 2.8 seconds. The
  rebuilds happen at the same points and read the same rules. Output is
  unchanged.

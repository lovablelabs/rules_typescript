### Fixed

- **A BUILD file rewritten with identical content just before a gazelle run no
  longer stops that run's record from being kept.** The racy window refused the
  record for any input changed within 2 s of the run's start, BUILD files
  included, though the record keys a walked BUILD file by its content, not its
  stat. A BUILD file's change now counts only when it falls inside the run or
  carries a whole-second stamp, so the next unchanged run replays in about
  0.3 s instead of running in full. A BUILD file the extension also reads by
  name, such as a `ts_config` owner's, still counts.

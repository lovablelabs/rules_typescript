### Fixed

- **A BUILD edit after a body edit that changes no import takes the Gazelle
  fast path.** The body edit's replay carried the run record to new
  walked-directory stat lines, and the build snapshot, tied to the old ones,
  stopped matching: every later hand edit to generated deps ran the full pass
  (about 15 s on a large workspace instead of 0.5 s). The record now carries
  the walk digest its snapshot was sealed with. BUILD output is unchanged.

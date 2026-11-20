### Fixed

- **A listing patch that drops many files no longer takes time quadratic in
  the program's size.** The patcher rebuilt its index of the program's edges
  once for each file it dropped and each edge it removed. A stale listing
  that had lost a directory of generated declarations, a few thousand files
  of a program with tens of thousands of edges, spent minutes patching on
  one core, where tsgo lists the program again in seconds. A removed edge now
  keeps its place in the listing until the patch takes its removals out
  together, and the index is rebuilt then, at most twice in a patch. A patch
  that drops 4,000 files from a program of about 12,000 edges went from 16 s
  to 0.1 s. Patched listings are unchanged.

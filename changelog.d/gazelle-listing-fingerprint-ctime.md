### Fixed

- **Gazelle no longer serves a stale listing after a source is rewritten in
  place with its mtime restored.** The listing cache took an entry as fresh
  while every read kept its mode, size, mtime and inode, so an equal-length
  import edit such as `./b` to `./c` with the old mtime put back kept the old
  edge, and the BUILD files differed from a run without the cache. A read's
  stat line now carries its ctime too, as the run record's already did, on
  file systems whose ctime moves with the write. A listing read under
  `node_modules` keeps no ctime, since pnpm's store links move it with no
  change of content. Listing entries and the reads and scan sums run records
  keep are computed again once after the upgrade.

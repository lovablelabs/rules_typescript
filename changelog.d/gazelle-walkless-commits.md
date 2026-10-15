### Changed

- **A BUILD-only edit Gazelle reverts, and a commit that touches no file the
  TypeScript Gazelle reads, no longer run the full pass.** The run record's fast
  path writes a reverted BUILD edit back as recorded once it proves Gazelle's own
  merge restores it, and passes directory entries no part of the run read (a Go
  or YAML file beside the walked sources). A full pass also saves listings off
  its critical path and patches a files-list projection beside the listing it
  derives from. Generated BUILD files are unchanged.

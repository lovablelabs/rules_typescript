### Changed

- **The Gazelle walk looks up each directory's info once.** A warm full pass on
  a large workspace spent about 1 s re-matching every ancestor against the
  exclude globs on each `walk.GetDirInfo` call, and 0.4 s computing parent
  directories of node_modules files it then skipped.

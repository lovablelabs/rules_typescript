### Changed

- **The run and resolve records replay on caches that hard-link the gazelle
  binary and tsgo.** Their stat lines now leave the ctime out, as a
  `node_modules` entry's already did: another hard link to the same inode moves
  the ctime, so a remote executor or a local cache sharing the inode refused
  every replay. A `node_modules` file the extension reads by name now leaves
  its ctime out too, as one it reaches through a listing did. Mode, size, mtime
  and inode still count, so a rewrite of a tool in place that restores its
  mtime goes unnoticed. Output is unchanged.

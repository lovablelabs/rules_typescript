### Changed

- **The run-record check stats a walked directory's entries through the
  directory's own descriptor.** On Linux (amd64 and arm64) each entry is read
  with `statx(2)` relative to the directory it was listed from, asking only for
  the type, mode, size, mtime, ctime and inode, so the kernel no longer resolves
  every path component again for each of the check's entries. Where `statx` is
  missing or refused, or another platform runs, the check stats each path as
  before. Output is unchanged.

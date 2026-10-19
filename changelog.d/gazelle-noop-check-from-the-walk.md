### Changed

- **The run record's check makes about 115k fewer of its 275k syscalls on a
  large workspace**, for an unchanged run and for a body edit's fast path. It
  reads its inputs in one parallel pass, and an input in a directory the walk
  already read takes its line from that read.

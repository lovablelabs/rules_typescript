### Changed

- **A no-op Gazelle run loads its run record about 12 ms faster.** Each
  recorded path is now a substring of the file as read rather than a string of
  its own, so decoding a record of about 100k paths makes a dozen allocations
  instead of one per path, and garbage collection is paused from the load to
  the end of the record check, unless memory reaches 1 GiB plus 16 times the
  record's size (or a lower `GOMEMLIMIT`). A no-op's peak RSS grows by about
  50 MB on a 22k-source tree. The record format is unchanged.

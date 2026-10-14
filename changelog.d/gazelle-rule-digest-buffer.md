### Changed

- **A Gazelle run that replays its resolve record digests its starting state
  faster.** The digest made one SHA-256 hasher per rule, program and cached
  listing, each with a 64 KiB write buffer, and allocated again for every
  integer it wrote. Each hasher now takes a 512-byte buffer and writes integers
  without allocating. On a large app's workspace, a hand edit to a resolved
  attribute such as a deleted `deps` list went from about 7.8 s to 7.4 s. The
  digest bytes are unchanged, so existing records still replay; generated BUILD
  files are unchanged.

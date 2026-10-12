### Changed

- **CI runs the nested-Bazel `core` leg as five legs.** The 17
  `gazelle_roundtrip` groups move to four `roundtrip-<n>` legs, balanced by
  measured time, so the slowest leg no longer runs 65 to 110 minutes on one
  runner. CI also runs in the merge queue.

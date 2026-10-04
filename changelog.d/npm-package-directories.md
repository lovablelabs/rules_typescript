### Changed

- **An npm package is one source artifact, not one target per file.** Each
  snapshot repository exposed every file of its package through a `glob`, so
  every file became a source target and a configured target at analysis;
  `typescript` alone is over 12,000. The extracted package directory is now
  the one input of its `NpmStore` copy, and a bin's missing script is found
  at fetch time. `deps(//tests/npm:node_modules)` configures 818 targets
  from npm repositories, down from 8,635. Bazel 9 tracks the directory by
  content by default; Bazel 7 and 8 need the startup environment variable
  `BAZEL_TRACK_SOURCE_DIRECTORIES=1`, or they warn per package and detect
  changes by mtime ([COMPATIBILITY.md](COMPATIBILITY.md)).

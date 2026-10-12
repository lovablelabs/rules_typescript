### Changed

- **A pull request's CI unit job runs only the targets its change can reach.**
  `tools/ci/affected_targets.sh` selects `rdeps(//..., <changed files>)` and
  falls back to `//...` whenever the build graph cannot answer: build files,
  module and lock files, patches, toolchain sources, CI files, and deleted or
  unowned files. Pushes still run `//...`, and every other job runs in full.

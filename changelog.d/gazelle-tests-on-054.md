### Changed

- **The documented bazel-gazelle floor is 0.51.3.** rules_go 0.62, which the
  llvm toolchain module pulls into every consumer's graph, requires it, so a
  workspace that declared 0.47.0 already resolved 0.51.3. `e2e/basic` declares
  0.51.3 with `--check_direct_dependencies=error` and runs most of the extension's
  tests and the golden test on it. The ruleset's own build and every
  integration and example workspace that runs Gazelle run 0.54.

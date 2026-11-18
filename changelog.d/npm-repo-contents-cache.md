### Changed

- **npm package repositories and the pnpm, tsgo and tools toolchain
  repositories are shared through Bazel's repo contents cache.** `npm_import`
  returns `repo_metadata(reproducible = True)`; `tools_toolchain_repo` and
  `tsgo_toolchain_repo` do when their download has an integrity, and
  `pnpm_repo` when every platform binary has a known sha256. A second output
  base or worktree links these repositories from
  `--repo_contents_cache` instead of extracting its own copy.

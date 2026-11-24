### Changed

- **npm package repositories and the pnpm, tsgo and tools toolchain
  repositories are shared through Bazel's repo contents cache.** `npm_import`
  returns `repo_metadata(reproducible = True)`, `tsgo_toolchain_repo` and
  `tools_toolchain_repo` too when their download has an integrity, and
  `pnpm_repo` when every platform binary has a known sha256. A second output
  base or worktree links these repositories from `--repo_contents_cache`
  instead of extracting its own copy. A fetch that sends a registry
  credential, a literal `.npmrc` one included, is not shared, and neither is
  one that reads a set `.npmrc` `${VAR}`: Bazel records the value of each
  variable a fetch reads beside the cache entry. Every `npm_import` and
  `tsgo_toolchain_repo` fetch reads `PNPM_CONFIG__AUTH`, so while it is set
  none of them is shared, even for packages that need no credential. No
  token reaches the shared cache or
  `--experimental_remote_repo_contents_cache`.

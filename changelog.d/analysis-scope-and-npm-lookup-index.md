### Changed

- **Analysis of a large app's tests and dev servers spends less CPU on
  package scopes and npm lookups.** `check_dev_server_npm_contexts` wrote one
  entry per (source, npm binding) pair, and the scope lookups scanned every
  package scope for each module. The check now decides each directory's shared
  lookup once, and scopes are indexed by directory: on a large app's test and
  dev server, cold analysis of the two targets drops from 12.7 s to 7.5 s of
  CPU. Actions and error messages are unchanged.

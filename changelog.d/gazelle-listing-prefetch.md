### Changed

- **Gazelle lists TypeScript programs in parallel during the walk and parses
  their listings faster.** On a full recursive update, while a directory is
  configured, the `tsconfig.json` listings of the subdirectories it will list
  run in the background, a few at a time, so the walk no longer waits on one
  `tsgo` run after another. The resolution
  trace and `--explainFiles` parsers match lines literally instead of through
  backtracking regular expressions, and per-run answers about codegen trees,
  output producers and nearest `package.json` files are memoized. Generated
  BUILD files are unchanged; a synthetic monorepo of 16,000 files regenerates
  in under a third of the time.

### Fixed

- **A vitest `ts_test` no longer creates npm links that node's walk up
  already resolves.** The launcher linked every demanded
  `<directory>/node_modules/<name>` in the test's private tree, then removed
  each one after the run, although the nearest `node_modules` entry above
  already selected the same store: all 440,362 links in a 16,000-module test
  were redundant, about 40 s of placement and 10 s of cleanup per test action
  on remote execution. It now skips a link when the nearest entry on the walk
  up is the same store, and places it as before otherwise. The build-time
  native view still links every free destination.

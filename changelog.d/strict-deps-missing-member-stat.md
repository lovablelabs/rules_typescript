### Fixed

- **A passing sandboxed type check no longer spends most of its time in the
  strict-deps check.** To name the declared npm package that provides a file the
  checker listed, `tsaction` tried the file's path under every declared package.
  A package that lacks the file was not skipped after its failed `stat`: it went
  on to a symlink resolution that `lstat`s every path component before reporting
  the same miss. The cost grew with npm files imported × npm packages declared ×
  path depth. A package without the file is now skipped after the one `stat`.

### Changed

- **A full Gazelle pass spends less time finding each package's
  `package_scopes`.** Every package that could take a scope-only `ts_compile`
  scanned every file of every listed program, cleaning each file's directory
  path again, so the work grew with packages times files. Each program now
  indexes its first-party sources by directory once, and a package reads only
  the directories under it. Generated BUILD files are unchanged.

### Breaking — ts_npm_package

- **`package_files` is removed.** The package's files are its store tree:
  `NpmPackageInfo.all_files` and the default outputs are `store`'s tree, and
  `package_root` is that tree's path. Delete the attribute from a
  hand-written `ts_npm_package`; the `npm_store` it names keeps its `files`.
- **`data = ["@npm//:<pkg>"]` stages the package's store tree.** Its files
  move in the runfiles from `<repo>/node_modules/<name>/` to
  `<ws>/<lockfile package>/node_modules/.pnpm/<key>/node_modules/<name>/`,
  e.g. `_main/node_modules/.pnpm/fsevents@2.3.3/node_modules/fsevents/`.
  Find a package's files by that path.

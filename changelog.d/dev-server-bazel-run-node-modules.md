### Fixed

- **`bazel run` of a `ts_dev_server` with `node_modules` finds its
  dependencies again.** The launcher prefers the runfiles manifest beside its
  executable, and the manifest lists the dev server's npm view only by the
  links inside it, so looking up the view directory failed with
  `runfiles lookup failed ... file does not exist` before the server started.
  A directory the manifest has no entry for now resolves through the runfiles
  tree beside the manifest, when Bazel built one.

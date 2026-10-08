### Fixed

- **A bazel-bin watcher event no longer resolves every declared file's
  realpath.** vite-plugin-bazel asked whether each event's path was declared,
  and a miss realpathed every declared path to compare; with thousands of
  declared files, the dev server's own writes under bazel-bin kept the server
  busy for minutes. A declared file is only compared against a canonical path;
  declared directories are still checked for every path.

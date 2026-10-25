### Fixed

- **The dev server serves a declared directory output whose files are
  symlinks to paths outside it.** When bazel-bin is mirrored with one link per
  file, Vite resolves a directory member to its link target, and the plugin
  rejected it as "resolved outside declared directory". In serve mode the
  plugin now maps the target back to the member that links to it and serves
  it at the member's path.

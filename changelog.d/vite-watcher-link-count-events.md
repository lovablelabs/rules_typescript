### Fixed

- **A remote executor's input staging no longer floods the bazel-bin watcher.**
  A watch on a symlinked file under bazel-bin followed the link to an inode
  that co-located actions on a remote executor link and unlink as they stage
  inputs, so it fired thousands of attribute events a second on files nobody
  edited. That kept the dev server busy and invalidated modules while it was
  still booting, and boots stalled. A link to a file outside bazel-bin is now
  watched through the target's directory (darwin under Node keeps the per-file
  watch), and a file event counts only when the file is gone or its device,
  inode, size or modification time changed.

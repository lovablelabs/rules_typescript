### Changed

- **vite-plugin-bazel no longer realpaths every declared file on each lookup,
  and no longer watches all of bazel-bin at start.** A canonical path that
  matched no declared file realpathed every declared file and directory, on
  every load and resolve. Their realpaths are now indexed, and indexed again
  when bazel-bin or the workspace resolves elsewhere, when a watched rebuild
  lands, or when an indexed match no longer resolves to the query. With
  outputs declared under bazel-bin, the watcher opens no watch at start for a
  directory there that leads to none of them, which on a large application was
  most of bazel-bin. Such a directory is watched once the server loads a file
  from it, or when it appears or is replaced later.

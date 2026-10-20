### Changed

- **The run-record check stats each read outside the walk once.** A stamped read
  in `node_modules` or outside every walked directory took one `lstat(2)` for its
  line and a second for its change stamp; both now come from the same `lstat`.
  Output is unchanged.

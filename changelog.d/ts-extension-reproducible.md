### Fixed

- **The `ts` extension no longer writes an entry to `MODULE.bazel.lock` when
  every compiler download is integrity-pinned.** It returned no
  `extension_metadata`, so Bazel recorded a hash of every file it read, the
  root `pnpm-lock.yaml` and `pnpm-workspace.yaml` among them: any two branches
  that each changed a dependency conflicted on those two lines, a conflict only
  `bazel mod deps --lockfile_mode=update` resolves. The extension is now
  reproducible whenever every platform's compiler carries an integrity, which
  is the case with `ts.tsgo(pnpm_lock = ...)` and with no `ts.tsgo()` call at
  all; the Go source modules are pinned by `go.sum` and the prebuilt tools by
  their own integrity table. `ts.tsgo(version = ...)` downloads the compiler
  unverified, so it keeps its lockfile entry. Run
  `bazel mod deps --lockfile_mode=update` once after updating to drop the
  stale entry.

### Added

- **`TS_TEST_SKIP_TMPDIR_CLEANUP=1` leaves the vitest launcher's staged
  trees under `TEST_TMPDIR` for Bazel to discard.** A remote executor drops
  `TEST_TMPDIR` with the action, so removing the tree there only costs time.
  Off by default, since a local `bazel test` keeps `TEST_TMPDIR` until the
  next run. See [The vitest Runner](docs/rules/ts-test.md#the-vitest-runner).

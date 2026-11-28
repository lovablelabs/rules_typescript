### Fixed

- **`--test_filter` selects tests on a vitest `ts_test`.** The vitest launcher
  ignored `TESTBRIDGE_TEST_ONLY`, so `bazel test --test_filter='case name'` ran
  every test of the target. It now passes the filter to vitest as
  `--testNamePattern`, as the node:test runner passes it as
  `--test-name-pattern`.

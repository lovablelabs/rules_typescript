### Fixed

- **A Workers-pool `ts_test` with tens of thousands of runtime files no longer
  fails `WranglerTestConfig` on remote execution with `Argument list too
  long`.** The action passes one `--runtime-file` per file, and a long
  command line now reaches the patch through a param file.

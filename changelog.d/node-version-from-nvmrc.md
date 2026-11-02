### Changed

- **A consumer's tests run under the Node its `.nvmrc` names.** rules_nodejs's
  `node` extension keeps the root module's `node.toolchain(name = "nodejs")`
  and ignores rules_typescript's, so
  `node.toolchain(name = "nodejs", node_version_from_nvmrc = "//:.nvmrc")`
  beside `bazel_dep(name = "rules_nodejs", version = "6.7.7")` in a consumer's
  `MODULE.bazel` is the runtime every `ts_test` and node build tool runs
  under, and an edit to the file moves it. With no call the runtime is the
  `24.22.1` this module pins: a default, not a decision for the consumer.
  `//tests/integration/node_version` pins the three cases; the quickstart's
  Version Pinning section has the lines.

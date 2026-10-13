### Changed

- **`ts_proto_library` runs protobuf's prebuilt protoc instead of compiling
  protobuf from source.** It resolves protoc through protobuf's proto
  toolchain, as Bazel 9's `proto_library` does, so
  `--@protobuf//bazel/toolchains:prefer_prebuilt_protoc=false` still selects
  the source build.
- **The `//ts:declarations`, `//ts:source_map`, `//ts:declaration_map`,
  `//ts:checkers` and `//ts:lib_check` flags no longer propagate into the exec
  configuration (`scope = "target"`).** A target built under another value of
  one of them reuses the exec tools, Go standard library and C++ runtimes
  already built rather than building them again. TypeScript built for the exec
  configuration (a `ts_binary` used as a tool, a `ts_codegen` generator) now
  always builds with these flags' defaults; target code keeps the values the
  build sets.

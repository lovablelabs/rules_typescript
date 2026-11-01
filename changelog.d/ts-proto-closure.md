### Added

- **`ts_proto_closure` generates a proto graph as one library.** It takes
  `protos`, generates each `proto_library` they reach except protobuf's
  well-known types, which protobuf-es imports from its runtime, and compiles
  the result as one source-mode `ts_compile`. A caller no longer mirrors the
  proto import graph in `ts_proto_library` `deps`.

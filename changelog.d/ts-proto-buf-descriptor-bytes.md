### Changed

- **`ts_proto_library` embeds the descriptor bytes `buf generate` does.**
  protoc's `CodeGeneratorRequest` is re-encoded the way buf v1.73.0 sends it
  to a plugin with a type filter before protoc-gen-es reads it: custom option
  values such as `google.api.http` and `buf.validate.field` take Go's
  deterministic field order, and empty options messages are dropped. A
  `_pb.ts` checked in from buf with `types` or `exclude_types` regenerates
  byte for byte; one from buf without a filter now differs wherever it had an
  `rpc ... {}`, whose empty `MethodOptions` buf keeps there.

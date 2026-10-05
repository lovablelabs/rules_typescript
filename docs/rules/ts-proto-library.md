# Protobuf TypeScript sources

`ts_proto_library` generates the direct sources of one `proto_library` with protobuf-es 2.12.0, then passes those TypeScript files to the existing source-mode `ts_compile` owner.

```starlark
ts_proto_library(name = "messages", proto = ":messages_proto", out_dir = "generated", tsconfig = "tsconfig.json", node_modules = ":node_modules", deps = ["@npm//:bufbuild_protobuf"], options = ["target=ts", "json_types=true"])
```

| Attribute | Contract |
| --- | --- |
| `proto` | One `proto_library` with direct sources; its graph owns imports and import-prefix mapping. |
| `out_dir` | Package-relative generated directory. Canonical proto import paths determine nested output paths. |
| `tsconfig`, `node_modules`, `deps`, `type_inputs` | Existing `ts_compile` inputs. Declare generated sibling libraries and the consumer's protobuf runtime in `deps`. |
| `options` | Options passed directly to protobuf-es, defaulting to `target=ts`. Supports `json_types=true` and `import_extension`. Output must remain TypeScript. |

The rule calls protobuf's `proto_common.compile` with its protoc executable and the declared protobuf-es plugin. The proto dependency enables protobuf's source-info flag to preserve comments. Only direct files are generated; imported TypeScript modules belong to their own generated libraries.

The Bazel target graph selects schemas. The rule does not interpret Buf configuration or apply managed schema options. Schema options affecting generated descriptors belong in the proto sources. Migration checks compare the exact generated path set and TypeScript text outside embedded descriptors, then compare decoded `FileDescriptorProto` messages with top-level `FileOptions` cleared and absent or empty descriptor-options messages treated equally. Decode custom extensions with their schemas so wire-field ordering does not count as a change; retain extension values, scalar presence, repeated-value order, and unknown fields. Other message presence remains significant.

## Generated BUILD targets

[Gazelle protobuf identities](../gazelle/directives.md#protobuf-generation-identities) generate wrappers from native proto rules. A common ancestor package owns each product output root; native schema packages retain their own targets. Different plugin options use different identities, while sibling dependencies remain inside each identity.

The selected tsconfig's compiler-observed ambient types use the same dependency projection as ordinary compiler rules. An existing compiler owner becomes a dependency. Eligible declarations without an owner, their observed closure and required package scopes become `type_inputs`; they remain compiler inputs without joining the generated runtime sources. Their existing Bazel visibility still applies.

## Invariants

1. Canonical proto import paths determine generated paths. The import-prefix fixture fails if generated sibling imports no longer resolve.
2. Each proto target owns only its direct generated files. The two-library fixture fails on duplicate ownership or missing imported sources.
3. Generated TypeScript uses the existing source-mode compiler and test runner. The JSON source-runtime fixture fails if emit-disabled generated sources cannot compile or execute.
4. Selected-config ambient inputs survive separate owner updates and repeated generation. `TestProtoAmbientSourceDependencySurvivesSeparateOwnerUpdates` fails if either a declared owner or an unowned declaration closure disappears.

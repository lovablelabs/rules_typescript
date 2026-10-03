# Canonical runtime admission

[RunfilesIdentity.tla](RunfilesIdentity.tla) defines canonical admission: each artifact has one canonical module coordinate. Its finite domain contains three coordinate-to-artifact relations (one coordinate, two coordinates sharing an artifact, or two distinct artifacts), alias absent/present and both renderers. [suite.json](suite.json) checks this domain, one mutation control and all twelve native trace seeds. The expected positive census is 12 initial, 24 distinct and 36 generated states; these are derived counts pending execution at this revision.

[CanonicalTrace.tla](CanonicalTrace.tla) exports bindings, alias membership, renderer and publication disposition through native TLC JSON. The [Go replay](../../ts/tools/runtimeview/conformance/replay.go) creates real regular files and calls `runtimeview.Build` or `runtimeview.Stage` using the trace's inputs and verdict. Distinct artifacts contain equal bytes, so byte equality cannot substitute for artifact identity. Rejection requires the canonical-identity diagnostic and no materialized canonical module; admission requires regular files at every canonical coordinate.

| Invariant | Failure prevented | Trace or control |
| --- | --- | --- |
| `DuplicateCanonicalOwnersReject` | One artifact receives two canonical module identities. | Four `trace_shared_*` seeds replay both renderers and alias states. `unchecked_canonical_owners.cfg` disables identity checking and must violate this invariant. |
| `UniqueCanonicalOwnersPublish` | Admission rejects one artifact or two distinct artifacts with unique canonical coordinates. | Eight `trace_single_*` and `trace_distinct_*` seeds replay both renderers and alias states. |
| `TypeOK` | State escapes the declared relation, alias, renderer or publication domain. | `runfiles_identity.cfg` checks every state in the finite domain. |

## Public gate

```sh
bazel test //tests/integration:gazelle_roundtrip_test --test_output=all
```

The [existing Go integration owner](../../tests/integration/runners/gazelle_roundtrip/conformance.go) runs canonical admission, [producer publication export and replay](../../ts/private/rules/conformance/README.md), then the existing integration journeys. ProducerPublication retains its model, mutation controls, native analysis traces and Node/Vitest execution witnesses. The public target declares the platform-selected remote JDK 21, model sources, both Go adapters and the TLC jar. The jar is the [official TLA+ VS Code distribution](https://raw.githubusercontent.com/tlaplus/vscode-tlaplus/96ad1f0b73619c56909928f4e1c2d7437060b8b3/tools/tla2tools.jar) at extension commit `96ad1f0b73619c56909928f4e1c2d7437060b8b3`. Its manifest identifies TLC source commit `30a4862fef489d2cd4a29096541cc35727d60897`. The adapters verify TLC's SHA256 `c953c663948041f6b198935952233aacc09bef81784c929acb26eae07bedc72c`.

Both adapters receive the same deadline derived from Bazel's `TEST_TIMEOUT`, the existing CPU reservation and the remaining cleanup allowance. The public target uses the `long` timeout category and `ci-merge-gate`; its Go owner rejects timeout overrides above the 45-minute gate limit. The existing harness owns process termination, installation and caches. Native replay disables test-result reuse and retries. Evidence remains in Bazel's undeclared outputs.

Acceptance requires successful finite-domain exhaustion with the declared census; native exit 12, the intended named invariant and a decoded native witness for each mutation or trace export; and agreement between every replayed trace verdict and the actual renderer. Producer publication additionally requires every exported analysis target to pass, and each exported native target to execute both required test bodies exactly once under directory and manifest runfiles. The gate's [runner](../../tools/tlatrace/run.go) rejects unexpected model failures, omitted states and census mismatches.

## Proof boundary

Canonical admission covers artifact-to-coordinate uniqueness and canonical materialization. The concrete fixtures for alias-link rendering, occupancy, replacement demands, source-copy removal, repository coordinates, config projection, discovery/sharding and native module resolution remain unchanged; those behaviors are outside this canonical model. Producer publication has its own [observation contract](../../ts/private/rules/conformance/README.md#observation-contract). These finite models make no claim about arbitrary layouts or dependency graphs.

The reduced gate has not been executed at this revision. Runtime performance and coverage are not measured.

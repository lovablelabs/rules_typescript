# Producer publication trace export

[ProducerPublication.tla](../../../../formal/runfiles_identity/ProducerPublication.tla)
defines the publication projection. This adapter exports native TLC witnesses
as Bazel analysis fixtures. `//tests/integration:gazelle_roundtrip_conformance_test`
runs them against real `ts_compile` producers and the production provider helpers.
Its Go owner runs canonical-admission conformance, exports producer traces,
then executes the exported analysis and native runtime targets in its existing
installed workspace.

The model contains 27 finite relation witnesses, including source/runtime
identity, direct and transitive publication, relocation, aliases, prior owner
records and scope shadowing. These are selected behavioral classes, not an
exhaustive claim about arbitrary layouts or dependency graphs. Two source-mode
producers may publish the same authored File under different owner labels;
the projection retains both records. Different producing source Files for one
runtime File conflict.

Six witnesses select a forwarded generated, emitted or opaque prior-provider
JavaScript root beside an ordinary local test, using Node or Vitest. Requested
input-to-runtime selection is separate from producing source/runtime/owner facts.
Vitest discovery retains requested input coordinates, including identity
pairs. Module configuration retains producing origins; an opaque publication
gets no invented origin.
The alias witness supplies two requested aliases of one runtime and explicitly
selects only one. The other alias must not enter discovery.

## Invocation ownership

`//ts/private/rules/conformance:export_producer_traces` accepts these explicit
inputs from the existing outer invocation owner:

| Flag | Input |
| --- | --- |
| `-java`, `-tlc` | Declared Java executable and the pinned TLC jar verified by [main.go](main.go). |
| `-models` | Directory containing the producer models, configs and `producer_suite.json`. |
| `-output` | Fresh directory for native evidence and the generated workspace fragment. |
| `-deadline` | The outer invocation's existing deadline, as an RFC3339 timestamp. |
| `-workers` | CPUs reserved by that invocation for model checking; trace exports use one worker. |
| `-cleanup` | That invocation's existing TLC wait allowance. |
| `-node-modules` | Existing harness importer label, such as `//:node_modules`. |
| `-node-types-dep`, `-vitest-dep` | Existing harness dependency labels, such as `@npm//:types_node` and `@npm//:vitest`. |

The exporter runs Java through [tools/tlatrace](../../../../tools/tlatrace).
It launches no Bazel process, installs no tool or package, and reads no
credential configuration. The outer owner retains the aggregate deadline and
process-group cleanup responsibility; context cancellation does not interrupt
blocking filesystem operations.

[The existing Go integration owner](../../../../tests/integration/runners/gazelle_roundtrip/conformance.go)
stages the exported workspace after its first Gazelle pass, when the normal
importer labels exist. It runs every target from `export.json`, then removes
the generated packages before the next Gazelle pass. Its harness retains the
Bazel executable, startup flags, repository and action caches and environment.
No credentials enter generated files or receipts.

`receipts.jsonl` records native model verdicts with model, config and trace
hashes. `export.json` reports `exported`, the expected analysis labels and count,
and native labels with their runner and required test bodies. The trace supplies
the witness count; omitted or duplicated witnesses fail export. The integration
owner requires a passing Bazel event for every exported target and retains
their test summaries separately from model verdicts. Raw Bazel events stay
in harness scratch because command-line events may contain invocation settings.

The public target declares the pinned TLC jar and platform-selected remote
JDK through [inputs.bzl](inputs.bzl). The same nested-test tags supply its CPU
reservation and TLC worker count. It derives one deadline from `TEST_TIMEOUT`
at runner entry, uses Bazel's `long` timeout category and rejects overrides above
45 minutes. Bazel owns whole-test termination; the exporter grants no additional
budget. Both model phases, generated files and native results are retained in
Bazel's undeclared outputs. Replay keeps build caches and disables test-result
reuse and retries.

## Observation contract

`ProducerPublicationTrace.tla` exports immutable input facts and the expected
publication result through native TLC `ALIAS` JSON. Go validates the wire
shape and writes those values to `cases.bzl`; it computes no expected source,
owner, path projection or admission result.

[replay_assertions.bzl](replay_assertions.bzl) binds model identities to the
actual producer Files and owner records. One generic assertion path compares
the production `runtime_mappings`, live closure, direct data/JavaScript,
canonical links and asset records with the exported result. Expected failures
require the diagnostic exported by the model. Ordinary asset observations
retain their original logical coordinate separately from the consumer's
publication path. They remain direct, copyable member inputs without module
`canonical_links`.

Normal graphs use real `ts_compile` producers. Literal provider fixtures are
limited to alias-wire inputs, conflicting origins and the prior owner shape.
The prior fixture matches commit `9aad440`: emitted JavaScript is absent from
`owner.files` and present in `transitive_js`. Malformed alias records use
acyclic physical symlinks to the actual runtime File so rejection tests reach
provider admission without requiring an invalid Bazel action graph.

Generated identity roots come from real `ts_codegen` using the existing
`tsaction stage` generator. A transparent runner wrapper records the actual
`ts_test` entry points, selected requested `runtime_inputs` and producing
`runtime_files` before invoking the shipped runner unchanged. The observer
reads the real test-files and launcher actions; for
Vitest it also reads discovery lists, runfiles symlinks and module references
from the generated configuration. Expected selection and source relations
come only from the native trace. A missing callback field is recorded as
absent and fails replay; the observer supplies no fallback mapping.

| Invariant | Failure prevented | Projection mutation control |
| --- | --- | --- |
| `ValidPublicationAccepted` | Data and dependency views reject one canonical placement. | `producer_duplicate_placement.cfg` |
| `InvalidFactsReject` | Conflicting source/alias facts, cycles or a real scope shadow are accepted. | `producer_accepted_{origin_conflict,alias_conflict,alias_cycle,scope_shadow}.cfg` |
| `ProducerOriginSurvives` | A publication becomes its own producing source. | `producer_reinterpreted_origin.cfg` |
| `ProducerOwnerSurvives` | Republishing assigns the consumer's owner context. | `producer_rebound_owner.cfg` |
| `CanonicalFileRemainsLive` | Publication loses the producer's canonical File. | Every accepted trace checks exact live membership. |
| `ExplicitCoordinatesSurvive` | Known assets lose their explicit consumer-local path. | `producer_dropped_asset.cfg` |
| `DirectPublicationMembership` | Foreign canonical dependencies become direct package contents. | `producer_promoted_dependency.cfg` |
| `OrdinaryMemberAssetRemainsCopyable` | Ordinary member assets acquire module-alias restrictions. | `producer_restricted_member_asset.cfg` |
| `OpaqueFactsRemainOpaque` | Old records acquire invented source associations. | `producer_inferred_opaque_origin.cfg` |
| `RequestedRootsRemainSelected` | A forwarded test root disappears while a local test still runs. | `producer_dropped_forwarded_root.cfg` |
| `DiscoveryUsesRequestedInputs` | Vitest loses generated or opaque roots, or leaks an unselected alias into discovery. | `producer_dropped_identity_discovery.cfg`, `producer_unselected_alias_discovered.cfg` |
| `RunnerOriginsStayProducing` | Discovery fabricates an origin for an opaque publication. | `producer_invented_opaque_root_origin.cfg` |
| `TypeOK` | Projection state escapes its declared domain. | The positive finite-domain check. |

The generic identity replay leaves compiler-action inputs, native module
loading and npm-store copying to their concrete boundary fixtures.

## Native execution boundary

Run the complete gate with:

```sh
bazel test //tests/integration:gazelle_roundtrip_conformance_test --test_output=all
```

The Go owner executes each exported native target with directory and manifest
runfiles. It selects the shipped Node or Vitest JUnit reporter and requires
one passing testcase for each exported body name in that target's own report:
`forwarded producer root executes` and `ordinary local root executes`.
A passing Bazel status alone cannot establish that the forwarded body ran.
The alias witness selects one alias; execution with both aliases selected
remains outside this native witness set.

## Validation status

This source has not been compiled, model-checked or replayed. The positive
manifest's expected census is derived from 27 initial witnesses and one
publication transition each; it is not an observed TLC result. Runtime and
analysis performance are not measured.

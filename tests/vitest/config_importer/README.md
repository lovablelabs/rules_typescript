# Vitest config in a sibling importer

The config loads `zod` and `minimatch` 10.2.4 from its importer while the test
keeps `minimatch` 9.0.9. The same runtime assertions exercise
`config_node_modules` and existing authored `data`.

| Invariant | Regression |
|---|---|
| Config settings take effect; package versions stay distinct and the test cannot resolve config-only `zod`. | `config_importer_test`, `authored_data_config_importer_test` |
| Full and partial generation preserve unmarked file and importer `data`, while stale generated edges disappear after import or config changes. | `TestGazelle_AuthoredConfigImporterRunfiles` |
| Literal pnpm importer records determine config ownership, including ancestor importers and shared package names. | `TestResolveEdges_AuthoredConfigImporterRunfiles` |
| Generated config owners and Workers settings retain their contracts. | `TestResolveEdges_GeneratedConfigStagesScalarFiles`, `TestResolveEdges_SiblingConfigKeepsWorkersAttributes` |
| Config-only npm packages cannot become direct test-program deps. | `config_dep_rejected_test` |
| node:test rejects the Vitest config importer attribute. | `config_importers_rejected_test` |
| The module importing the pool determines `workers_pool`, including a nested helper while the test declares a different version. | `TestResolveEdges_WorkersPoolOwnerIsTheImportingHelper` |
| Full and partial generation withdraw a removed pool import and preserve authored data; only Wrangler preparation requires a unique pool owner. | `TestGazelle_WorkersPoolOriginAndWithdrawal` |
| A workspace wrapper's member store owns its files while compiler-observed source paths retain the Workers owner, Wrangler and Istanbul settings. | `TestGazelle_WorkersPoolOriginAndWithdrawal` |
| Separate declaration and runtime exports preserve explicitly kept pool, Wrangler, Istanbul and coverage dependency settings across full and partial generation, without copying member sources. | `TestGazelle_WorkersPoolOriginAndWithdrawal` |
| Known type-only paths stage no config helpers or importer stores; an independent runtime path to the same helper still selects its pool and participates in conflict detection. | `TestResolveEdges_KnownTypePathsDoNotSelectWorkersPool`, `TestGazelle_WorkersPoolOriginAndWithdrawal` |
| A runtime package import resolved through a declaration retains its npm link without staging the declaration's import closure. | `TestResolveEdges_ConfigMemberNameKeepsItsImporter` |
| A runtime import resolved through a generated declaration retains its producer's JavaScript and data outputs. | `TestResolveEdges_ConfigImportThroughGeneratedDeclarationRetainsRuntimeOutputs`, `generatedConfigStagesScalarRuntime` |
| Wrangler preparation and runtime use the helper's pool link and store; another test pool contributes no action inputs. | `nested_config_pool_action_test`, `nested_config_other_pool_action_test`, `nested_config_pool_test` |
| Original attributes remain accepted and Wrangler uses the test chain's pool link and store; the worker runs with its authored config and data. | `//tests/workers_nested/test:source_worker_retains_runtime_module_identity_test`, `//tests/workers_nested/test:source_worker_test` |
| A pool member reached through a compile dependency supplies the same link and store to preparation and runtime. | `legacy_transitive_member_pool_action_test`, `legacy_transitive_member_pool_test` |
| Legacy preparation skips a nearer declared but unstaged pool and uses the ancestor link present at runtime. | `legacy_unstaged_nearer_pool_action_test`, `legacy_unstaged_nearer_pool_test` |
| An unlinked explicit pool owner fails without falling back to the test pool; node:test rejects the selection. | `unlinked_config_pool_owner_analysis_test`, `workers_pool_rejected_test` |

The alternative pool in the analysis fixture has distinct synthetic link and
store files. It is never executed; the non-manual runtime fixture uses the
existing Workers lockfile's pool. No installed package versions are changed.

Wrappers whose declarations hide their runtime pool need the
[explicit Workers settings](../../../docs/rules/ts-test.md#a-workers-pool) before
regeneration; automatic selection cannot recover that runtime choice.

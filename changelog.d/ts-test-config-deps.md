### Added

- **`ts_test` takes `config_deps`, the npm packages its config imports.** Each
  importer in `config_node_modules` that links one of them then adds only that
  link, the package's store closure and the hoist links the closure names to
  the runfiles, not its whole closure. Gazelle writes `config_deps` from the
  config's static imports, next to `config_node_modules`, and not when
  `config_node_modules` or the rule is kept. A test that borrows a large
  importer for a shared vitest config no longer carries every package of that
  importer: in the Lovable monorepo, three tests that use the web app's vitest
  config drop from about 1,520-1,610 npm packages to 590-760.
  - A package the config reaches only through `require`, `require.resolve`, an
    alias path or a name resolved from `root` needs a hand-written,
    kept `config_deps` entry.
  - A `config_deps` entry that no listed importer links to the same snapshot
    fails analysis, as does a workspace member, whose link target belongs in
    `config_node_modules`.
  - Member links, and a `config_node_modules` list without `config_deps`, are
    staged whole as before.

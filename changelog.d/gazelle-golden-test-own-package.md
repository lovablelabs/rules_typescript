### Fixed

- **`@rules_typescript//gazelle` loads again under Gazelle 0.54.** Gazelle
  0.54's `gazelle_generation_test` fails on `gazelle_timeout_seconds`, which the
  Gazelle golden test passed, so every consumer on 0.54 lost
  `@rules_typescript//gazelle:typescript`. The test now lives in
  `//gazelle/golden`, a package consumers never load.

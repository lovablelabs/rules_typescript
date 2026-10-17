### Fixed

- **A partial run under `-index=lazy` indexes the directories the extension
  reads.** The extension now names in `RelsToIndex` every directory it reads
  while generating a package, so Gazelle indexes them before Resolve; a test whose
  vitest config sits in another package no longer loses its `wrangler_config`
  and `workers_pool`. A directory read only while Gazelle indexes a package it
  does not generate is not named. `-index=lazy` is Gazelle 2's default.

### Fixed

- **A `pnpm install` now invalidates the Gazelle listing cache
  (`-ts_listing_cache`).** A listing kept only its first-party reads, and the
  probes into `node_modules` are not first-party, so a package installed after
  a listing cached it as unresolved stayed unresolved and Gazelle removed the
  dep from the BUILD file. Each listing now also reads the root
  `node_modules/.modules.yaml` and `node_modules/.pnpm/lock.yaml`, the files the
  run record already checks, so any install lists every program again, and
  entries written before this change are not reused.

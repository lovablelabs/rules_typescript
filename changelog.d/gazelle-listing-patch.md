### Changed

- **A stale listing is patched for added and deleted files and for new
  imports, instead of waiting on tsgo to list the whole program again.** With
  `-ts_listing_cache`, Gazelle keeps every resolution whose probes still find
  what they found, resolves a new relative specifier as tsgo's bundler
  resolution does, reuses a bare specifier's resolution from another importer
  with the same module format, package scope and `node_modules`, and adds or
  drops the roots the `include` globs match. Anything else (`paths` or
  `exports` targets no importer resolved yet, `rootDirs`, `moduleSuffixes`, a
  `files` list, `allowJs`, a changed config) is listed by tsgo as before.
  `-ts_listing_patch_verify` lists every patched program with tsgo too and
  fails on any difference. Generated BUILD files are unchanged.

### Fixed

- **`-ts_listing_patch_verify` compares every fact a later patch consumes.** The
  verifier skipped a category the fresh listing left empty, so an extra edge
  there compared equal, merged explicit and implicit `types` entries, and did
  not compare unresolved imports and their probes, the refusal, or the traced
  resolution facts (resolution kind, `rootDirs`, relative and substituted
  targets, `paths` substitutions). A patched listing may still keep resolution
  facts from an import the edit dropped; it may not lack one tsgo traces.

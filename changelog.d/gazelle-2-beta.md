### Added

- **`gazelle_typescript` runs under Gazelle 2.0.0-beta.2.** Gazelle 2's
  `language/proto` no longer exports a v1 `NewLanguage`, so the extension
  failed to compile; it now computes a `proto_library`'s import paths itself.
  Under Gazelle 2's default `-r=auto` a run with no directory named is
  recursive, so it writes run records and takes the fast path, and the resolve
  record digests the `_gazelle_lang` and `_gazelle_kind` attributes and the
  `language/defaults` configuration that Gazelle 2 adds instead of refusing to
  record. `e2e/gazelle_latest` runs the extension's tests under Gazelle
  2.0.0-beta.2.

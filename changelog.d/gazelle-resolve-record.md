### Changed

- **A TypeScript-only Gazelle run whose `Resolve` starts from a recorded state
  writes back that run's resolved BUILD files instead of resolving again.**
  With `-ts_listing_cache`, a full recursive `fix` or `diff` run of
  `gazelle_typescript` also keeps a resolve record, after gazelle-rs's narrowed
  run record: a digest of the state the first `Resolve` sees, with the BUILD
  files and log that `Resolve` and `AfterResolvingDeps` left. A hand edit to an
  attribute Gazelle resolves, such as a deleted `deps` list, leaves that state
  as it was, so on a large app's workspace such an edit went from about 14 s
  to 8 s. Any other change resolves in full, about a second slower for the
  digest and the record. `-ts_resolve_record=verify` resolves anyway and fails on any
  difference in BUILD files or log, and `-ts_resolve_record=off` disables it.

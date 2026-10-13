### Changed

- **A TypeScript-only Gazelle run after an edit that adds an import between
  sources one rule already compiles replays the last run's record instead of
  running the full pass.** After gazelle-rs's import fast path, the run
  record also keeps what `Resolve` did with each listing's edges. When the only
  changes are edits to existing sources, the run patches the listings that read
  them and proves from those facts that every new import resolves as an edge
  from another source of the rule into the same file already does, so no BUILD
  file or log line changes; a body edit replays the same way. On a large app's
  workspace a one-import edit went from about 14 s to about N s. Anything the
  proof does not cover runs in full, and `-ts_verbose` names why.
  `-ts_fastpath=verify` checks each replay against the full pass, its exit
  status and log included, and `-ts_fastpath=off` disables it. A run that
  replays its `Resolve` phase from a resolve record now records that phase's
  reads in its run record too.

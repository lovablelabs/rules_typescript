### Added

- **`-ts_fastpath=only` answers "is every BUILD file provably in sync?"
  without ever running the full pass.** When the run record or the fast path
  replays, the run is the one `-ts_fastpath=on` makes. When neither does, it
  logs `typescript: fast path: declined (<code>): <detail>`, writes no BUILD
  file, listing or record and exits with status 3, so a caller such as a
  pre-push hook running `-mode=diff` can fall back to something cheaper than
  the full pass and knows why. `no-record` is the code when the cache holds no
  record of a run with these arguments; every other code is the fast path's
  own reason. An `only` run never makes a record from a full pass, so the
  first record comes from the same run without the flag. With
  `RULES_TYPESCRIPT_GAZELLE_TRACE` naming a file, a declined run appends
  `declined <code>` to it. The flag is an error on a run that keeps no run
  record and with `-ts_run_record=off` or `verify`. A fast-path BUILD edit
  whose write fails now puts back the file it was writing as well as the ones
  before it.

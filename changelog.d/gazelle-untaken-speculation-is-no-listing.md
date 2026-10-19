### Changed

- **A source added beside an existing one takes Gazelle's fast path in a
  package whose speculative listing goes unused.** The walk lists a program's
  sources ahead of time with each declaration's JavaScript twin; when no rule
  asks for those roots, the run drops that listing, and its record names only
  the listings a rule used.

### Changed

- **CI's Bazel jobs share one set of caches that only main saves, and every
  download retries.** All jobs on an OS restore the same repository and disk
  cache entries; the disk cache key no longer changes with every commit, and a
  pull request restores without saving, so it no longer evicts main's entries
  from the 10 GB Actions cache. The disk caches are trimmed to a bound before
  they are saved. A transient 5xx from a GitHub archive no longer fails a cold
  fetch. A new push to a pull request cancels the run on its previous head, and
  the unit and integration legs run on the runners the `CI_LARGE_RUNNER` and
  `CI_LARGE_MACOS_RUNNER` repository variables name, when set.

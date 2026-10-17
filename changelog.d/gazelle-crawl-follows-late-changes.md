### Fixed

- **Gazelle follows workspace members a crawl reached before the lockfile
  loaded.** The first crawls run while the npm lockfile is still loading, so
  `@npm//:` labels they reached stayed unmapped until an unrelated package
  edit dropped the memo; a partial run could miss the member's package. A new
  lockfile now drops the memo.
- **Gazelle re-crawls after another language merges into a package with
  TypeScript disabled.** No TypeScript callback sees such a merge, so the old
  memo kept following that package's previous rules until some crawled
  package was edited.

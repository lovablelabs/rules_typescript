### Fixed

- **Gazelle follows a change found in the middle of a crawl through rules the
  same crawl had already skipped as unchanged.** The crawl went on without
  them, so a partial run could miss a package the change made reachable.
- **Gazelle re-crawls after another language changes a root rule when the
  root has TypeScript disabled.** No callback runs after the root's merge, so
  the change was never reviewed.

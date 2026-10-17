### Changed

- **Gazelle no longer re-crawls BUILD dependencies after an unchanged package.**
  The walk's dependency crawl memo was dropped whenever a callback touched, or
  stored a rule in, a package the crawl had read, so most crawls re-followed
  the same rules. The memo now lasts the whole walk, and the next crawl
  rereads each package touched since.

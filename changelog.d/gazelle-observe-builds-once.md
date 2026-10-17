### Changed

- **Gazelle no longer re-crawls BUILD dependencies after an unchanged package.**
  The walk's dependency crawl memo was dropped whenever a callback touched, or
  stored a rule in, a package the crawl had read, so most crawls re-followed
  the same rules. The memo now lasts the whole walk: the next crawl rereads
  each package touched since, and drops the memo only if a load there would
  store something or a crawled rule's kind, references or deps changed.
  Storing a rule at a label the crawl found without one still drops it.

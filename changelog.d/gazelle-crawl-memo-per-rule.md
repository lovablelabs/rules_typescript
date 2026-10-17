### Changed

- **Gazelle re-crawls only the BUILD rules a change reaches.** The walk's
  dependency crawl memo was still dropped whole when a crawled rule's
  references changed, when a rule was stored at a label the crawl had found
  empty, and at every `exports_files` or `package` call in a BUILD file. It
  now drops only the changed rules and the crawled rules that reach them, and
  a crawl no longer queues labels it has already crawled.

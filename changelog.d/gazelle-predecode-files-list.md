### Changed

- **Gazelle decodes files-list projection listings ahead of the walk.** The
  predecode skipped the cache keys of files-list projections, so the walk
  decoded each of those entries itself when it reached them. They are now
  decoded ahead like every other listing the last full run took, and each
  entry decoded ahead interns its listing and candidate strings at once,
  as the walk does on a hit. Generated BUILD files are unchanged.

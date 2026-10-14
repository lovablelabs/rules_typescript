### Changed

- **A cold Gazelle run lists a large program's three listings two at a
  time, and decodes tsgo's trace as tsgo prints it.** A package's tsconfig
  was listed on its own, then over the sources its BUILD file keeps, then
  over its sources with each listed declaration's JavaScript twin, each
  listing waiting for the one before. The last two need only the first, so
  Gazelle now starts the third beside the second, and a later listing asked
  for the same roots takes it. Projection configs no longer keep other
  listings waiting: only a tsconfig whose `include` names `.json` files,
  which could root one, lists alone. A subdirectory whose BUILD file has
  directives is listed ahead of the walk unless one is an `exclude` or a
  `generation_mode`. tsgo's output is decoded from the pipe instead of
  buffered whole, which took a 14,000-root app's listing from about 1.5 GB
  of heap to about 250 MB. On a large app's workspace a run with an empty
  listing cache went from about 49 s to about 32 s. Generated BUILD files
  are unchanged.

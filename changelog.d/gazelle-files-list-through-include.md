### Changed

- **A cold Gazelle run no longer waits on tsgo's scan of a long `files`
  list.** tsgo explains each root of a `files` list by a scan of the whole
  list, so projecting a tsconfig onto its own roots plus a few extra sources
  listed several times slower than the tsconfig itself: 23 s instead of 5 s
  for a 14,000-root app, twice per cold run. With `-ts_listing_cache`, when
  the list holds every root of the tsconfig's own listing from the same run,
  tsgo now lists a projection that keeps the tsconfig's `include` and names
  only the extra sources, which roots the same files. The listing is cached
  under the `files` list as before. On a large app's workspace a run with an
  empty listing cache went from about 90 s to about 51 s. A tsconfig with a
  `files` list of its own, a dropped root, or an extra source whose extension
  tsgo ranks above a root's of the same name goes to tsgo as before.
  `-ts_listing_patch_verify` also lists the `files` list and fails on any
  difference. Generated BUILD files are unchanged.

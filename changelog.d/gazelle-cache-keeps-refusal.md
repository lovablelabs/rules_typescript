### Fixed

- **A program tsgo refuses stays refused on a listing cache hit.** The cache
  entry did not keep the refusal, so a hit handed back an empty listed program:
  a rule keeping its sources over a `tsconfig.json` whose `files` entry is
  missing was generated where a cache-less run fails. The entry keeps the
  refusal, and a `files` entry tsgo did not find is a read of the listing, so
  writing it lists the program again instead of reusing the listing without
  it. Entries written before the change miss once.

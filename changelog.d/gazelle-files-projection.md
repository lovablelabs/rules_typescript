### Changed

- **A program listed with a `files` list is patched when the list gains or
  loses a root, instead of waiting on tsgo to list it again.** With
  `-ts_listing_cache`, the listing of a tsconfig projected onto extra sources
  is keyed on the projection without its `files` list. A new list starts from
  a cached listing of the same projection, nearest list first, and the roots
  it gained or lost are patched as added and deleted files are; a script in
  the list that tsgo does not root stays a non-root. Such a listing is no
  longer staled by a file appearing under the base config's `include` globs,
  which the projection replaces. A gained root that is missing, not
  TypeScript, or has references or pragmas is listed by tsgo as before.
  `-ts_listing_patch_verify` lists these programs with tsgo too and fails on
  any difference. Generated BUILD files are unchanged.

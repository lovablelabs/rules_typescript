### Changed

- **The Gazelle walk remembers which rule declares each generated file.** A
  warm full pass on a large workspace walked every candidate file's ancestor
  packages three or four times per program selection to find the rule that
  outputs it or the codegen tree above it. These answers now live in the memo
  Resolve already keeps, for one walk callback at most. Output is unchanged.

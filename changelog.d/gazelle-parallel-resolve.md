### Changed

- **Gazelle selects program imports in parallel during Resolve.** Workers
  select every program, and every refresh's program, on read-only copies of the
  store. The serial pass takes each result, in directory order, only when what
  it read is unchanged, and otherwise selects again as before. BUILD output is
  unchanged.

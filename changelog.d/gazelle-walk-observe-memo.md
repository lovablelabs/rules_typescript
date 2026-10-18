### Changed

- **The Gazelle walk parses each config owner's BUILD file once and collects a
  program's source directories once.** A warm full pass on a large workspace
  re-parsed the nearest owning BUILD file for every tsconfig and `extends` base
  it resolved, and re-derived the directories of the whole program for every
  rule it observed the program for. Output is unchanged.

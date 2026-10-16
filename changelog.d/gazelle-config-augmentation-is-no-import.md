### Fixed

- **A config's `declare module` augmentation of a generated or overridden
  module no longer stages it as a config import.** When Gazelle replaced such
  an edge with the file a producer or `# gazelle:resolve` override names, it
  gave the new edge the import kind, so a `vitest.config.*` that only augments
  the module got the producer's or override's label in `deps` and the file in
  `config_srcs`. An augmentation of an authored file never did. The edge now
  keeps the kind the compiler listed; an import of the same module still wins.
  Only the source-built compiler lists augmentations.

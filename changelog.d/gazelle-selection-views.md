### Changed

- **Gazelle's Resolve phase copies less for each rule.** A selection of a
  program's inputs holds exactly the edges and resolution candidates it keeps,
  with its own index of those edges by importer, instead of a clone of every
  candidate and a second grouping of its edges. Each Resolve walk reads
  candidates through one index of positions instead of copying every candidate
  of the program into a map. Generated BUILD files are unchanged.

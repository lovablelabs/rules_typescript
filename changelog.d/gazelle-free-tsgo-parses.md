### Changed

- **Gazelle frees tsgo's parses once the walk's listings finish.** The
  in-process listing kept every file it parsed until exit; it now drops them
  once the walk's listings stop, and a listing in the Resolve phase parses
  again what it reads. Generated BUILD files are unchanged.

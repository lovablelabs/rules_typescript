### Changed

- **The Gazelle fast path replays an edit to a JSON module's content and a new
  directory of TypeScript sources.** A JSON file a listing holds as a module,
  and takes no compiler options from, moves no listing whatever it holds, so an
  edit to it replays without listing again; under `-ts_verbose`, which logs
  tsgo's diagnostics, it still runs in full. A new directory that holds only
  library sources joins its package's `ts_compile` through the existing
  new-source proof, standing with the nearest sources of that rule above it,
  provided no directory between them holds a file the extension or Gazelle
  reads by its name, such as a package manifest or config.
- **The fast path declines before patching a listing when the patch is bound
  to fail its checks**: a changed source that drops an import of a source the
  edit keeps, or a new source of a kind its listing gives an implicit package
  import, such as a JSX runtime. A listing cache entry now leads with the
  edges and source facts those checks read, so such a decline decodes no more
  of the entry, and the process hands back what it decoded before the full
  pass runs.

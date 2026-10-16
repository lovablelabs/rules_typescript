### Changed

- **An edit that changes no import of a TypeScript source replays the last run
  without reading a listing.** The run record keeps a sum of each source's scan
  (its import specifiers and their resolution counts, triple-slash references,
  pragmas, JSX and module status), and a run whose only moved inputs are sources
  with an unchanged sum replays the record instead of patching and rewriting the
  listings it is in. As before, a replay logs the diagnostics tsgo printed for
  the recorded run.
  Generated BUILD files are unchanged.
- **A new directory with no TypeScript or BUILD file, or an edit in a directory
  with no TypeScript, no longer makes Gazelle resolve in full.** The resolve
  record's state leaves out the file names of directories with no TypeScript
  source, tsconfig.json, package.json, program or generated rule, and every
  read the Resolve phase makes of such a directory is recorded and must read
  the same for the record to replay.
- **A new directory holding nothing Gazelle reads, such as a new Go package, no
  longer makes an edit run in full.** The fast path accepts a gained directory
  with no BUILD file, link, program source or configuration, unless the last
  run read every name of its parent (a package's data), looked up a path in it,
  a BUILD file declares an output there, or the walk skips it or a directory
  above it (an exclude, `.bazelignore`, `.git`, or a REPO.bazel
  `ignore_directories` pattern, matched as Gazelle matches it); later runs watch
  it as walked.
- **A new subpath import (`#...`) takes the fast path as a relative import
  does.** It names no npm package, so its dep is its target's owner. Bare
  specifiers, which may name a workspace member, still run in full.
- **An edit that tsgo reads as an import change no longer replays or patches
  a stale listing.** The scanner now agrees with tsgo on a regular expression
  after an `if (...)` head, on module status (a top-level `import(` makes no
  module, an `import.meta` anywhere does), on pragmas (on any line of a comment,
  in any case, the last of a repeated one winning), and on directives and
  pragmas after the first token, which tsgo ignores. A listing patch refuses a
  source with an import declaration inside braces other than a module
  declaration's, a `from` after no import clause, a regular expression right
  after a `}`, or a scan that misses an import tsgo resolved. Listing entries an
  earlier version wrote are listed again once.
- **The body-edit replay also passes over sources the scanner may misread**: a
  `resolution-mode` attribute, a namespace import alias, an import declaration
  where no statement starts (after an `if (...)` head, `else`, `do` or a label),
  or a string, template, regular expression, comment or JSX text that reads like
  an import or export (under 1% of an application's sources). An
  edit to one patches its listings as before.
- **A cold run allocates about a third less** (trace lines formatted into reused
  buffers, listing files encoded straight to disk, fewer copies in selection and
  scanning). Generated BUILD files are unchanged.

### Added

- **`ts_refresh_tsconfig(generated_sources = True)` reads a workspace member
  whose own program compiles generated TypeScript sources through the member's
  store copy.** Refresh refused every editor project whose dependencies reached
  such a member, so a consumer had to commit a checkout copy of the generated
  file. `ts_compile` now names those members to the editor action, which writes
  one exact `compilerOptions.paths` entry per member specifier the compiler
  resolved, pointing at the file the build read. Refresh still refuses a member
  link whose closure holds any other generated input (the member's generated
  JSON included); a target that reaches the member through another target, even
  with a direct link too; `node16` or `nodenext` module resolution and nonempty
  `moduleSuffixes`; a type reference naming the member; and a member specifier
  that resolves to two files, fails to resolve in the build, or resolves outside
  the build output.

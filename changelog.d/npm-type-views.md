### Changed

- **Type checking and linting stage each npm package's type view instead of
  its tree.** Each store target declares a view of its tree with the files
  TypeScript's resolution reads: `package.json`, JSON, TypeScript sources and
  declarations, and JavaScript with no declaration TypeScript tries in its
  place. Views sit at the same paths under `node_modules.types/`, with twins
  of the edge, hoist and importer links. TsEmit, TsgoCheck, TsgoDeclare and
  TsLint stage the views; a TsLint whose lint configuration names a
  `node_modules` in its `data`, and every runtime action, stage the trees.
  `--//ts:type_views=false` restores the trees. Over a monorepo's 940
  programs, TsgoCheck and TsLint stage a third fewer files and spend 14% less
  worker time. `TsInfo` gains `npm_view_files`; a `TsInfo` built without it
  makes its dependents' programs read the trees.

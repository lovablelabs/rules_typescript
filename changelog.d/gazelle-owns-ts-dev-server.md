### Breaking — gazelle

- **Gazelle owns the `ts_dev_server` it names `dev`.** It used to write one
  only when the BUILD file had none, with `entry_point`, `port = 5173`,
  `plugin` and `visibility` and no `node_modules`, and to leave one already
  there alone. It now writes the target on every run in an application
  package, one whose program lists a library source and either has an authored
  `index.html` or lists a `main.ts[x]` or `app.ts[x]`, naming it `dev_server`
  in a package named `dev`. It replaces `entry_point`, `node_modules` (the
  nearest lockfile importer's tree), `plugin` and `visibility`, writes no
  `port`, and keeps every other attribute as written. In every other directory
  where it runs program discovery it withdraws a `ts_dev_server` under that
  name, hand-written or not. The edit: put `# keep` above a hand-written dev
  server outside an application package, or one whose `entry_point`,
  `node_modules`, `plugin` or `visibility` differs from what Gazelle writes, or
  give it another name. See the Dev Server guide.

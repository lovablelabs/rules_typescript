### Fixed

- **`ts_dev_server` serves a workspace member's source whose npm dependency
  only the member's own importer links.** The app importer no longer has to
  link every package a member declares: the dev app stages the member
  importer's links and resolves the member's bare specifiers through them. A
  name the app importer links to a different store, or two members link to
  different stores, is still rejected.

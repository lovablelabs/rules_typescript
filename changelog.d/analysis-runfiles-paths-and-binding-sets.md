### Changed

- **Analysis of `ts_test` targets spends less CPU, and their launcher configs
  are smaller.** `npm_binding_sets` held one copy of a bindings map per lookup
  directory even when many directories resolved to the same map: a large web
  test wrote 1,093 sets of which 6 were distinct, 17 MB of its 19 MB launcher
  config. Sets are now also shared by content. Hiding a runfiles path beneath
  another took one string join per ancestor of every path; one sorted pass now
  finds them. `ts_test` flattens its runfiles once for both the package-scope
  placement and the module check, unless scopes were staged in between, and
  `rlocation_path` and `runtime_scope_destinations` build their strings
  without splitting and re-joining whole paths. Actions are unchanged apart
  from the binding-set indices, which resolve to the same maps.

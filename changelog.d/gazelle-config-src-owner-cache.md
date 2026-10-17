### Changed

- **Gazelle no longer rebuilds a compiler owner's input map for every query.**
  Finding the owner of each scope in a rule's config closure, and each
  dependency-supply check, rebuilt the owner's whole input map from its
  `srcs`, `package_scopes` and `type_inputs`, so the cost grew with
  config-owning rules times their shared config closure. The map is now
  memoized per owner while one rule resolves and across the completion of
  every program after Resolve, and rebuilt when the owner's input lists, the
  stored rules, a rule list or a BUILD file change. BUILD output is unchanged.

### Changed

- **Gazelle's TypeScript pass spends less time rescanning the whole
  repository per package.** A package's `package_scopes` candidates are taken
  only from program files under that package, the fallback owner search for a
  package scope skips rules in other packages before inspecting them, and the
  path-containment check no longer allocates a string per comparison.
  Generated BUILD files are unchanged.

### Changed

- **Configuration no longer stages source modules from its type inputs.**
  A TsConfig action's inputs drop the `.ts`, `.tsx`, `.mts`, `.cts` and
  JavaScript sources among its type inputs: the implementations source-mode
  dependencies publish and source files in `type_inputs`. `tsgo --showConfig`
  matches the chain's `include` only to decide which of the target's own
  sources the written config appends, and the config keeps the chain's
  patterns. The dropped files are passed by path, so a relative `types` entry
  naming one resolves as before. Package scopes, authored declarations and
  generated directories remain inputs. Type validation, declaration emission
  and lint still read the implementations.

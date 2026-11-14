### Changed

- **The generated-mode `tsconfig.json` loads before the first refresh.** It
  references a tracked `tsconfig.editor.json`, which references
  `./.bazel/tsconfig/<target>.json`. Vite and Vitest load one level of
  references, so they no longer fail with `Tsconfig not found` on a checkout
  that has not run refresh. Vite's transform applies no tsconfig options to
  the package's files before or after refresh, where the old wrapper applied
  the installed project's options after refresh; set the ones it needs in the
  Vite config. Gazelle keeps a package's top-level `tsconfig.editor.json` out of
  its data srcs, as it does `tsconfig.json`.
  To adopt it, move the `.bazel/tsconfig` references from `tsconfig.json` into
  `tsconfig.editor.json` and reference that file instead; see
  [IDE setup](docs/getting-started/ide-setup.md).

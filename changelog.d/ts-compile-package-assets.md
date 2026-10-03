### Breaking — ts_compile

- Move foreign standalone JSON assets from `ts_compile.srcs` to `data` to
  retain package-local placement in source mode. For example, a member at
  `//packages/shared` uses `data = ["//:metadata.json"]` to package
  `shared/metadata.json`. JSON modules stay in `srcs`:
  source mode keeps their original paths for relative imports, and workspace
  packages reject modules outside the member's layout. Existing non-JSON
  assets in `srcs` remain supported. The new `data` attribute uses the same
  asset staging in both emit modes; `ts_test.data` remains extra runfiles.

- Move `package.json` files used only as package metadata from `srcs` to
  `package_scopes`, so runtime targets follow emitted modules. Keep a manifest
  imported as a JSON module in `srcs`; its authored contents are preserved,
  and emitted consumers reject targets that conflict with the published modules.

- Emitted consumers accept previous `owners` records without `runtime_files`
  and preserve their published Files and records. Optional immutable `(source
  File, runtime File)` pairs identify runtime modules for runfiles admission
  and package-scope projection. An empty tuple means no runtime mappings;
  an absent field supplies no mapping evidence. Consumer admission does not
  prove that relative imports resolve.

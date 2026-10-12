### Fixed

- **A vitest `ts_test` stages its private runtime tree in about half the
  time.** The launcher ran `MkdirAll` and `Remove` for every entry of a
  freshly created tree, and staged each runtime module as a link it then
  replaced with a copy through a temporary file and a rename. It now creates
  each directory once, skips the removal in a fresh tree, and copies a module
  with an absolute target straight to its path; npm placement walks up once
  per directory instead of once per demand. On a large app's test (30,251
  entries, 16,865 modules, 440,212 npm demands) staging fell from 2.4–5.0 s
  to 0.9–2.1 s and npm placement from 1.2–2.1 s to 0.7–1.0 s per test action
  on remote execution. The staged tree is unchanged.

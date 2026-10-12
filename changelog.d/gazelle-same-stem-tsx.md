### Fixed

- **Gazelle fails on a `.tsx` that a same-named `.ts` drops from the program,
  instead of leaving it out of every target.** TypeScript's `include` expansion
  keeps one file per path without its extension, `.ts` over `.tsx`, so
  `foo.test.tsx` beside `foo.test.ts` was in no `ts_test` and never ran. The run
  now names each pair and the tsconfig that lists the `.ts`; rename one file, or
  `# gazelle:exclude` the `.tsx` to keep it out on purpose.

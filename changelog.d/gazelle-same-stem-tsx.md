### Breaking — Gazelle

- **A `.tsx` beside a same-named `.ts` that one tsconfig program compiles fails
  the run.** TypeScript's `include` expansion keeps one file per path without its
  extension, `.ts` over `.tsx`, so `foo.test.tsx` beside `foo.test.ts` was in no
  `ts_test` and never ran; a `files` list naming both put two files that
  compile to the same outputs in one target. The run names each pair and the
  tsconfig. Rename one file of the pair, or `# gazelle:exclude` the `.tsx` to
  keep it out on purpose. A `.tsx` the tsconfig's own `include` or `exclude`
  leaves out, or another program lists, is not affected.

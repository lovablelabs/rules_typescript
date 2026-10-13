### Breaking — gazelle

- **A `.tsx` beside a same-named `.ts` fails the run when one tsconfig program
  includes or lists both.** TypeScript's `include` expansion keeps one file per
  path without its extension, `.ts` over `.tsx`, so `foo.test.tsx` beside
  `foo.test.ts` was in no `ts_test` and never ran; a program that lists both,
  through `files` or an import, put two files that write the same outputs in one
  target. The run names each pair and the tsconfig. Rename one file of the pair;
  `# gazelle:exclude` a dropped `.tsx` to keep it out on purpose. A `.tsx` the
  tsconfig's own `include` or `exclude` leaves out, or another program lists, is
  not affected.

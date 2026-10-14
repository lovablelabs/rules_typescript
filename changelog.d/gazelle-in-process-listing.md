### Changed

- **Gazelle lists programs in its own process when tsgo is a release of the
  typescript-go it links in.** Each listing ran a tsgo process that parsed
  every file of its program from scratch, and a large workspace's cold run
  lists about a hundred programs. When the toolchain's tsgo has a
  `package.json` naming the same TypeScript version as the linked-in
  typescript-go, Gazelle runs the listing's command line in process with the
  same output and exit status, and parses each file once for every listing of
  the run. A source-built tsgo, another version, or an `incremental` tsconfig
  lists through tsgo as before; `-ts_verbose` says which ran and why.
  `-ts_listing_inproc=verify` lists each program both ways and fails on any
  difference, `-ts_listing_inproc=off` always runs tsgo. The tsgo toolchain
  carries the release's `package.json` into the runfiles beside the binary.
  Generated BUILD files are unchanged.

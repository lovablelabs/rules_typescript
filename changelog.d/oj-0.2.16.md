### Fixed

- **The default oj dev server moves from 0.2.5 to 0.2.16.** Among the fixes:
  a dependency's extensionless relative import in Start SSR resolves Vite-style
  instead of answering 500 with `ERR_MODULE_NOT_FOUND`; a dependency's
  `browser` field remaps its own relative imports; linked workspace packages
  outside the app root are watched for HMR; a plugin-driven `server.restart()`
  reaps every descendant process; plugins see Vite's resolved `server.fs`,
  `server` defaults and absolute `cacheDir`; build plugins can spawn workers;
  idle engines return memory; and `?v=`/`?t=` edits no longer write a new code
  cache entry each. oj 0.2.16 replaces `deno_snapshots` and `deno_process` with
  its own `oj_deno_snapshots` and `oj_deno_process` forks; the V8, deno_core and
  Cranelift pins are unchanged; the snapshot fork's build script reads its
  manifest directory at run time, so the compiled script embeds no absolute
  path. The Start fallback-renderer backport is dropped (fixed upstream in
  0.2.9); the code cache, plugin-resolved file HMR and `fs.watch` dispatch
  patches are carried onto 0.2.16.

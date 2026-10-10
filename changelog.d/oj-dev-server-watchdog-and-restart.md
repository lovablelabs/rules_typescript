### Fixed

- **A parked oj call's deadline no longer stops another call's JavaScript.**
  The watchdog behind a call awaiting a promise fired at the call's own
  deadline and terminated whatever JavaScript was running, so an SSR request
  failed with "execution terminated" when another call timed out under load.
  It now fires 10 s past the deadline, only for a wedged event loop
  (`oj/oj-js-parked-watchdog-grace.patch`, lovablelabs/oj#347).
- **A plugin's `server.restart()` restores the Start client bundle from
  cache** instead of rebundling it. With the cache under a symlinked
  `bazel-bin`, the bundle's key covered oj's own `start/manifest.ts`, which
  every boot rewrites (`oj/oj-cache-start-bundle-stable-key.patch`); and the
  restarted process inherited the `process.env` writes of the app's Vite
  config, which changed the key again. A restart now starts with the
  environment oj was launched with (`oj/oj-server-restart-startup-env.patch`,
  lovablelabs/oj#348).

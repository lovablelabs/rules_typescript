### Breaking — ts_dev_server

- A workspace member the application imports is no longer pre-bundled, so it
  must ship ES modules. A member whose compile emits CommonJS fails in the
  browser and in server-side rendering with `require is not defined`,
  `exports is not defined` or a missing export; set its tsconfig's `module`
  to an ES kind such as `esnext`. A CommonJS dependency only a member imports
  is no longer discovered either; name it as `<member> > <dependency>` in
  `optimizeDeps.include` from a plugin's `config` hook.

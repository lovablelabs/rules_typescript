### Fixed

- **`ts_dev_server` on oj no longer hangs starting an app whose bazel-bin holds
  thousands of watched entries.** oj's `fs.watch` matched every event against
  every live watcher, opening both files each time, on the one notify thread
  that also registers new watches. With vite-plugin-bazel watching each
  directory and linked file under bazel-bin, that thread never caught up, and
  the plugin host blocked in its next `fs.watch` call before
  `configureServer` returned. Events now reach only the watchers indexed under
  their path, its ancestors, or its file identity.

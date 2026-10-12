### Changed

- **Every CI job that runs Bazel keeps disk and bazelisk caches of its own,
  restores one shared repository cache per OS, and retries failed downloads.**
  One job's cache going missing no longer turns every other job's build cold,
  a pull request's second push starts from its first, and a transient 5xx from
  a GitHub archive no longer fails a cold fetch. A new push to a pull request
  cancels the run on its previous head. The unit legs and the integration legs
  run on the runners the `CI_LARGE_RUNNER` and `CI_LARGE_MACOS_RUNNER`
  repository variables name, when set.

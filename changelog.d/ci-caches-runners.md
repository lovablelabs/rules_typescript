### Changed

- **Every CI job that runs Bazel keeps repository, disk and bazelisk caches of
  its own, and pull requests save theirs.** One job's cache going missing no
  longer turns every other job's build cold, and a pull request's second push
  starts from its first. A new push to a pull request cancels the run on its
  previous head. The unit legs and the integration legs run on the runners the
  `CI_LARGE_RUNNER` and `CI_LARGE_MACOS_RUNNER` repository variables name, when
  set.

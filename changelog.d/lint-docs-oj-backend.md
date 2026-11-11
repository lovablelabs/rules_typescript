### Changed

- **The lint guide says `tool_env` can bind oj as oxlint's type-aware backend.** oxlint starts `$OXLINT_TSGOLINT_PATH headless`; oj 0.4.12 and newer answers that call, so `tool_env = {"//tools:oj": "OXLINT_TSGOLINT_PATH"}` stands in for `@npm//:oxlint-tsgolint_bin`. Documentation only: `tool_env` already took any executable label.

## Unreleased

### Features

* **pr-review-watch:** add opt-in lifecycle mode (`--events`) for durable,
  head-bound reviewer waits: `--state` cursor, `--head`, `--repo`, exact
  `--authors`, and `--wait-for all|any`.

  Lifecycle mode collects typed `review`, `review_comment`, and `issue_comment`
  events for GitHub (via `gh`) and Forgejo (REST), labels each with its evidence
  binding (`commit`, `stable-head`, `none`), and prints a versioned JSON status
  object with documented exit codes `0` ready, `1` error, `2` timeout, `3` head
  changed, `4` closed.

  A submitted review counts only when its commit matches the observed head;
  pending and dismissed reviews never count; inline comments are evidence only.
  A top-level comment first seen after the baseline counts as explicit
  stable-head evidence and is never claimed to be commit-bound. The state file is
  written atomically with mode `0600`, bound to platform/repo/PR/head/authors,
  and refuses malformed, symlinked, or mismatched state instead of resetting it.
  The head is re-read after every fetch, and `review-watch: armed` is emitted on
  stderr only once the baseline is durable.

  The round deadline cancels in-flight HTTP/CLI reads and rejects late readiness;
  three consecutive observation failures stop with error while preserving the
  cursor. Results include the last observed head. Forgejo pagination follows
  server-clamped pages until empty and fails closed at its safety cap.

  The pre-existing new-root-comment mode is unchanged when `--events` is absent.

## [1.0.2](https://github.com/djdembeck/mira-pr-tools/compare/v1.0.1...v1.0.2) (2026-07-26)

## [1.1.0](https://github.com/djdembeck/mira-pr-tools/compare/v1.0.2...v1.1.0) (2026-08-08)


### Features

* **mira-review-parser:** add additional authors filter ([#6](https://github.com/djdembeck/mira-pr-tools/issues/6)) ([1de3dc6](https://github.com/djdembeck/mira-pr-tools/commit/1de3dc6db32d70b52771d7d3af3349a5f3efb2c0))

## [1.0.2](https://github.com/djdembeck/mira-pr-tools/compare/v1.0.1...v1.0.2) (2026-07-26)


### Bug Fixes

* **forgejo:** post threaded review replies with path/position so replies stay nested ([#3](https://github.com/djdembeck/mira-pr-tools/issues/3)) ([9bfbb7c](https://github.com/djdembeck/mira-pr-tools/commit/9bfbb7c88fe17cdae12394bcd09435a1af960693))

## [1.0.1](https://github.com/djdembeck/mira-pr-tools/compare/v1.0.0...v1.0.1) (2026-07-24)

## 1.0.0 (2026-07-12)

# pr-review-tools

[![CI](https://github.com/djdembeck/pr-review-tools/actions/workflows/ci.yml/badge.svg)](https://github.com/djdembeck/pr-review-tools/actions/workflows/ci.yml)
[![Latest release](https://img.shields.io/github/v/release/djdembeck/pr-review-tools)](https://github.com/djdembeck/pr-review-tools/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Close the PR-review loop on GitHub or Forgejo with zero Go dependencies, for reviews from ANY reviewer — bots or humans.

pr-review-tools provides three standalone Go binaries for AI agent workflows and developers who process PR reviews programmatically. The parser turns review comments into structured JSON or a consensus markdown summary; the reply tool posts custom per-comment replies or Mira-bot reject/acknowledge feedback, including batch operations, and resolves review threads on GitHub; the watch tool persistently polls a PR and exits as soon as a new review comment appears, so agent workflows can block on reviewers instead of polling manually. The feedback loop is deterministic and does not require an LLM per interaction.

The default relevance contract for the parser: **root comments that are open, not outdated, and not authored by you** — from every reviewer. If it cannot determine who *you* are, the parser fails with an error rather than passing everything through unfiltered (opt out explicitly with `--include-self`). It auto-detects GitHub or Forgejo from the repository's `origin` remote. GitHub uses the `gh` CLI; Forgejo uses its REST API and environment variables. All binaries are pure standard-library Go, so there are no external Go dependencies to download.

## Table of Contents

- [Why](#why)
- [Install / Running](#install--running)
- [Usage](#usage)
- [Platform detection and configuration](#platform-detection-and-configuration)
- [Output schema](#output-schema)
- [Building](#building)
- [Contributing](#contributing)
- [License](#license)

## Why

Reviewers post comments on PRs — automation bots like [Mira](https://github.com/mira-reviewer/mira) or `nimuebot`, and human reviewers — but there is no programmatic way for AI agent workflows (like `/fixup`) to see only the feedback that still matters and communicate back which findings were addressed and which were dismissed. This repo closes that loop: the parser extracts the relevant comments (filtering out resolved, outdated, and your own replies), and the reply tool posts a short custom note per comment and resolves the thread so it stays closed. Mira-authored comments additionally support the `@bot-name reject — <reason>` / acknowledge templates that feed Mira's learning loop.

The design separates parsing, acting, and waiting: `pr-review-parser` extracts review data, `pr-review-reply` posts the feedback, and `pr-review-watch` blocks until a reviewer posts new comments. Together they provide a parse-then-act pipeline for teams that need to process many findings without replying manually to every comment.

## Install / Running

The documented install path builds the binaries from source. There is no Docker image or pre-built binary distribution in this repository.

Prerequisites:

- Go 1.24
- `git`, with the PR repository available as the current checkout and an `origin` remote
- `make` for the recommended build command
- `gh` CLI and `GITHUB_TOKEN` for GitHub repositories; `gh` handles GitHub authentication
- `FORGEJO_URL` and `FORGEJO_TOKEN` for Forgejo repositories

From the repository root:

```bash
make build
```

This creates `bin/pr-review-parser`, `bin/pr-review-reply`, and `bin/pr-review-watch`. Set the credentials for the platform selected by the current repository's remote before running any binary.

If you prefer Go's module install path, it also compiles the commands locally and places them in `$GOPATH/bin` (normally `~/go/bin`):

```bash
go install github.com/djdembeck/pr-review-tools/cmd/pr-review-parser@latest
go install github.com/djdembeck/pr-review-tools/cmd/pr-review-reply@latest
go install github.com/djdembeck/pr-review-tools/cmd/pr-review-watch@latest
```

Make sure `$GOPATH/bin` is on your `$PATH`. Re-run those commands to update; there is no automatic update mechanism. See [GitHub Releases](https://github.com/djdembeck/pr-review-tools/releases) for changelogs.

<details>
<summary>Pin a module install to a specific version</summary>

Replace `v1.0.0` with the tag you need:

```bash
go install github.com/djdembeck/pr-review-tools/cmd/pr-review-parser@v1.0.0
go install github.com/djdembeck/pr-review-tools/cmd/pr-review-reply@v1.0.0
```

</details>

## Usage

The examples below use the binaries produced by `make build`. If you used `go install`, invoke `pr-review-parser`, `pr-review-reply`, and `pr-review-watch` without the `bin/` prefix. Run the commands from a checkout of the PR repository so the tools can inspect `git remote get-url origin`.

### Parsing

Parse the relevant review comments as JSON (the default: open, not outdated, non-self root comments from all reviewers):

```bash
bin/pr-review-parser 123
```

The parser writes a `ParsedComment[]` JSON array to stdout and progress messages to stderr.

Generate an alphabetically grouped consensus summary, include resolved threads, and restrict to specific reviewers:

```bash
bin/pr-review-parser 123 \
  --format consensus \
  --include-resolved \
  --authors nimuebot,human-reviewer
```

Because root comments from **any** reviewer are included by default, the parser applies a deterministic trust classification to the agent-instruction-bearing `agentPrompt` field: an author is trusted **only** when its login is an exact, case-insensitive match (after trim) against an entry in the `--trusted-authors` CSV flag. There is no bot-suffix rule and no hardcoded allowlist. For untrusted authors `agentPrompt` is `null` and `isTrusted` is `false`, so a PR reviewer cannot inject agent instructions. Trusted authors' prompts are emitted as found; consensus output marks each row as trusted or untrusted.

> **Behavior change:** unlike the previous release, logins such as `miracodeai-bot` or `nimuebot` are **NOT** trusted by default — the old rule that trusted any login ending in `bot` has been removed. You MUST pass your review bot's exact login via `--trusted-authors` to receive its agent prompts; forgetting the flag suppresses all `agentPrompt` output (comments still appear, with `isTrusted: false`).

```bash
# Trust your review bot's exact login so its agent prompts are emitted.
bin/pr-review-parser 123 --trusted-authors miracodeai-bot
```

On follow-up passes after fixes were pushed, only see feedback newer than the last pushed state (any ONE of the three):

```bash
bin/pr-review-parser 123 --since-last-push     # comments after the upstream tracking branch
bin/pr-review-parser 123 --since-last-commit   # comments after HEAD
bin/pr-review-parser 123 --since abc123        # comments after any git ref, or an RFC3339 timestamp
```

### Watching

Block until a reviewer posts: the watch tool persistently polls the PR and exits as soon as a **new** review comment appears.

```bash
bin/pr-review-watch 123                 # default: 15m timeout, 30s poll, all reviewers
bin/pr-review-watch 123 --timeout 5m --poll 10s --authors miracodeai-bot,nimuebot
```

Exit codes: `0` = new review comment found (emitted as JSON, or consensus markdown with `--format consensus`, on stdout); `1` = usage/setup/fetch error; `2` = timed out.

Baseline semantics: the first fetch captures the currently relevant comments (open, not outdated, non-self root comments — the same contract as the parser), keyed by comment ID. Only comments that appear **after** the watch starts count; pre-existing comments do not trigger success. New root comments trigger; a reply inside an existing thread does not. Transient poll errors (e.g. a network blip) are logged to stderr and the watch keeps polling until the deadline; a baseline fetch failure is fatal.

#### Lifecycle mode: durable, head-bound reviewer waits

The mode above answers "did a new comment appear?". An orchestrator that must wait for **named** reviewers on a **specific commit** — and survive a CI repair that moves the head, a restart, or a gap between two waits — needs lifecycle mode. It is opt-in with `--events`; without it every flag, output, and exit code above is unchanged.

```bash
bin/pr-review-watch 123 \
  --events \
  --state .git/pr-review/round-1-abc1234.json \
  --head 1234abcd…full-sha \
  --repo owner/repo \
  --authors miracodeai-bot,nimuebot \
  --timeout 30m --poll 30s
```

`--state`, `--head`, `--repo`, and `--authors` are required. `--wait-for all|any` defaults to `all`; `any` is an explicit opt-in. `--trusted-authors` stays a **separate** trust list for agent prompts and is never implied by `--authors`. Authors are matched by exact, case-insensitive login; there is no substring or `*bot` matching. Defaults in this mode are a 30m total wait and a 30s poll; each network request also has its own 30s deadline.

Exit codes:

| Code | Meaning |
|---|---|
| `0` | `ready` — the wait condition was satisfied |
| `1` | setup, fetch, or state error (including a rejected state file) |
| `2` | `timeout` — the deadline passed; progress is preserved |
| `3` | `head-changed` — the PR head is no longer the requested `--head` |
| `4` | `closed` — the PR is closed or merged |

stdout is a single versioned JSON object, printed in every outcome (including error and timeout). stderr carries progress and diagnostics:

```json
{
  "version": 1,
  "status": "ready",
  "platform": "github",
  "repo": "owner/repo",
  "pr": 123,
  "head": "1234abcd…",
  "observedHead": "1234abcd…",
  "statePath": ".git/pr-review/round-1-abc1234.json",
  "waitFor": "all",
  "authors": ["miracodeai-bot", "nimuebot"],
  "responded": ["miracodeai-bot", "nimuebot"],
  "outstanding": [],
  "events": [
    {
      "id": "PRR_kwDO…",
      "kind": "review",
      "author": "miracodeai-bot",
      "state": "COMMENTED",
      "commitId": "1234abcd…",
      "createdAt": "2026-01-01T00:00:00Z",
      "url": "https://…",
      "binding": "commit",
      "baseline": false,
      "body": "…"
    }
  ],
  "baselineEvents": 0,
  "polls": 2,
  "elapsedMs": 31000
}
```

**What counts as a response.** `all` is satisfied only when every selected author has either a submitted review whose commit equals `--head`, or a top-level PR conversation comment first observed **after** the baseline. Event kinds are `review`, `review_comment` (an inline root comment), and `issue_comment`. `binding` records the evidence quality:

- `commit` — a submitted review explicitly tied to `--head`. The commit is the proof, so a review already present at the baseline still counts.
- `stable-head` — a top-level comment first seen as new between two identical head snapshots. This is temporal evidence: it is **never** claimed to be commit-bound, because the platform gives top-level comments no commit. This is how a bot that only publishes a summary can satisfy the wait.
- `none` — no current-head binding: stale review commits, inline comments, and anything captured in the baseline.

Pending and dismissed reviews never count. Inline comments are collected as evidence but never complete an author — one comment in a multi-comment review does not prove the review finished. Approval state, CI status, and thread resolution are **not** readiness. Comment bodies are stored verbatim and never executed.

**Durability and resume.** The state file is the cursor: it binds to platform, repository, PR, head, `--authors`, and `--wait-for`, and it stores collected events, seen IDs, and the poll count. It is written atomically (temp file, fsync, rename) with mode `0600`, and contains no tokens or headers.

- The baseline is captured and durably saved **before** the first wait proceeds; the tool prints `review-watch: armed` on stderr only after that save. Start CI (or any other head-moving work) on that marker, not before.
- A timeout saves progress and exits `2`. Resuming the same `--state` does not rebaseline, so events that landed while no watcher was running — including during a CI repair — still count.
- A malformed, symlinked, group/other-writable, foreign-owner, wrong-version, or differently-bound state file is a hard error (exit `1`). The file is never silently reset or rewritten. To start watching a new head, use a **new** state path per round, e.g. `round-<n>-<head7>.json`.
- The head is read before and after every event fetch. Events collected across a head move are discarded rather than credited to the old head, and the round ends as `head-changed`.
- Transient API failures are retried with the cursor intact; three consecutive failed observations stop with error (`1`), rather than waiting out missing CLI/authentication setup. A round with only API failures also reports error, not missing reviews.
- `--timeout` bounds the whole round, including in-flight paginated HTTP/CLI reads. Deadline-expired observations never report ready. `observedHead` reports the last successfully read head, including on ready, timeout, and later errors.
- Forgejo review and conversation pagination continues until an empty page, even when the server clamps the requested page size. Hitting the 50-page safety cap is an incomplete-observation error, never partial readiness.

On GitHub this mode uses the `gh` CLI (GraphQL for head state, reviews, and review threads; the REST paginated issue-comment endpoint). On Forgejo it uses the REST API with `FORGEJO_URL`/`FORGEJO_TOKEN`, reading review `state`/`dismissed`/`commit_id`/`submitted_at`, per-review inline comments, and top-level issue comments. `--repo` selects the receiving repository; the platform is still detected from the `origin` remote, so a watch cannot be redirected to another host by the flag. All subprocess calls use argument vectors, never shell strings.

### Replied to every reviewer

Post a short custom reply to one comment and resolve its thread (GitHub):

```bash
bin/pr-review-reply 123 \
  --reply 3564917980 \
  --body "Fixed in abc123 — the null check now lives in the middleware." \
  --and-resolve
```

Resolve a thread without replying:

```bash
bin/pr-review-reply 123 --resolve 3564917980
```

Preview a batch reply without posting, with machine-readable results:

```bash
bin/pr-review-reply 123 \
  --batch-reply findings.json \
  --dry-run \
  --format json
```

The batch file is a JSON array of objects:

```json
[
  { "id": "3564917980", "body": "Dismissed: duplicate of the middleware check.", "resolve": true }
]
```

A missing `"resolve"` field (or `false`) leaves the thread open.

Mira-bot templates (for `isMira: true` comments) still work exactly as before — reject feeds Mira's learning loop, acknowledge records acceptance:

```bash
# Reject a false positive with the Mira learning-loop signal.
bin/pr-review-reply 123 --reject 3564917980 --reason "Auth at middleware layer"

# Acknowledge a valid finding and record the fixing commit. The tool posts a
# static template ("Acknowledged — valid finding. …"); --note appends short
# extra context after it — do not pass an acknowledgment sentence as the note.
bin/pr-review-reply 123 --acknowledge 3564917980 --commit abc123 --note "Covered by middleware tests"

# Let the tool find the active Mira bot from existing PR comments.
bin/pr-review-reply 123 --detect-bot
```

Batch reject/acknowledge files use the same array shape with `reason`/`note` instead of `body`, plus optional `"resolve": true`.

Use `pr-review-reply --help` for the complete option list. The available actions are `--reply`, `--resolve`, `--reject`, `--acknowledge`, `--batch-reply`, `--batch-reject`, `--batch-acknowledge`, and `--detect-bot`; common options include `--body`, `--and-resolve`, `--bot-name`, `--commit`, `--note`, `--dry-run`, and `--format json`.

### Forgejo limitations

- **Resolving is unsupported.** The Forgejo instance this project targets (Forgejo 16.0.3 / gitea-1.22.0 API) exposes no conversation-resolve endpoint, so `--resolve`, `--and-resolve`, and batch `"resolve": true` fail with an explicit error (`resolving review conversations is not supported by this Forgejo instance`). Post the reply without resolve; the loop carries forward via `--since-last-push`.
- Forgejo has no comment threading linkage on this instance, so your replies resurface as root comments — that is why the parser excludes self-authored comments by default (opt back in with `--include-self`).

## Platform detection and configuration

Both binaries inspect `git remote get-url origin` and select a backend automatically:

- A remote containing `github.com` selects GitHub and uses the `gh` CLI.
- A remote containing `git.theiahd.nl` selects Forgejo and uses `FORGEJO_URL` plus `FORGEJO_TOKEN` for API access.
- Any other host defaults to GitHub.

For Forgejo review comments, replies preserve thread placement by using the review comment path and position when the API requires it. If a threaded endpoint is unavailable for a summary comment, the tool falls back to an issue comment.

## Output schema

`pr-review-parser` supports `--format json` (the default) and `--format consensus`. Consensus output is human-readable markdown grouped by file in alphabetical order. JSON output contains one object per parsed root comment; every field is present, with `null` used for optional values that were absent in the source comment.

A parsed comment has this shape:

```json
{
  "id": "3564917980",
  "file": "src/auth.ts",
  "lineStart": 42,
  "lineEnd": 45,
  "category": "Bug",
  "severity": "blocker",
  "title": "Missing null check",
  "body": "...",
  "author": "miracodeai-bot",
  "isMira": true,
  "isTrusted": true,
  "suggestion": null,
  "agentPrompt": null,
  "diffHunk": null,
  "isResolved": false,
  "isOutdated": false,
  "createdAt": "2026-07-11T...",
  "threadId": "PRRT_kwDOAbc123",
  "threadReplies": 0
}
```

In this example `isTrusted` is `true` because the parser was run with `--trusted-authors miracodeai-bot`; without the flag, `isTrusted` is `false` for every author.

`threadId` is the platform review-thread identifier (always `null` on Forgejo) and is what the reply tool uses to resolve threads. `isOutdated` marks threads whose diff position is stale (GitHub only today).

`isTrusted` reports the deterministic trust classification of the author: `true` only when the login is an exact, case-insensitive match (after trim) against an entry in `--trusted-authors` — there is no bot-suffix rule and no hardcoded allowlist. `agentPrompt` is only emitted for trusted authors — it is `null` for everyone else, even when their comment body contains an agent-prompt block. This keeps the any-author default from becoming an instruction-injection channel.

The reply tool accepts `--format json` for reply results. Successful or failed results include the comment ID, action, generated body, and `success`; `error` and `replyUrl` are included when applicable. Resolve results appear as a second entry with `"action": "resolve"`.

## Building

`make build` builds all three commands:

```bash
make build
```

The equivalent direct commands are:

```bash
go build -o bin/pr-review-parser ./cmd/pr-review-parser
go build -o bin/pr-review-reply ./cmd/pr-review-reply
go build -o bin/pr-review-watch ./cmd/pr-review-watch
```

The repository has no external Go dependencies. For local checks, use:

```bash
gofmt -w .
go vet ./...
golangci-lint run
make build
go test -count=1 ./...
```

## Contributing

Open an issue or pull request on [GitHub](https://github.com/djdembeck/pr-review-tools). Keep changes focused, use [Conventional Commits](https://www.conventionalcommits.org/), and target the `main` branch; releases use semantic-version tags and are generated from commit messages.

Before submitting a change, run the formatting, vet, lint, build, and test commands in [Building](#building). Changes to Go dependencies must leave `go mod tidy` clean. CI also runs those checks on pull requests.

## License

MIT. See [LICENSE](LICENSE).

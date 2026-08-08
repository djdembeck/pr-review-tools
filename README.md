# mira-pr-tools

[![CI](https://github.com/djdembeck/mira-pr-tools/actions/workflows/ci.yml/badge.svg)](https://github.com/djdembeck/mira-pr-tools/actions/workflows/ci.yml)
[![Latest release](https://img.shields.io/github/v/release/djdembeck/mira-pr-tools)](https://github.com/djdembeck/mira-pr-tools/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Close Mira's PR-review loop on GitHub or Forgejo with zero Go dependencies and batch feedback.

mira-pr-tools provides two standalone Go binaries for AI agent workflows and developers who use [Mira](https://github.com/mira-reviewer/mira) for automated PR reviews. The parser turns Mira review comments into structured JSON or a consensus markdown summary; the reply tool posts reject or acknowledge feedback, including batch operations. The feedback loop is deterministic and does not require an LLM per interaction.

The tools auto-detect GitHub or Forgejo from the repository's `origin` remote. GitHub uses the `gh` CLI; Forgejo uses its REST API and environment variables. The project is active, and both binaries are pure standard-library Go, so there are no external Go dependencies to download.

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

Mira's review bot posts comments on PRs, but there is no programmatic way for AI agent workflows (like `/fixup`) to communicate back which findings were valid and which were false positives. This repo provides tools that post `@bot-name reject — <reason>` replies on false-positive threads and acknowledgments on valid ones, feeding Mira's learning loop without requiring an LLM for each interaction.

The two-binary design separates parsing from acting: `mira-review-parser` extracts review data, and `mira-review-reply` posts the feedback. Together they provide a parse-then-act pipeline for teams that need to process many findings without replying manually to every comment.

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

This creates `bin/mira-review-parser` and `bin/mira-review-reply`. Set the credentials for the platform selected by the current repository's remote before running either binary.

If you prefer Go's module install path, it also compiles the commands locally and places them in `$GOPATH/bin` (normally `~/go/bin`):

```bash
go install github.com/djdembeck/mira-pr-tools/cmd/mira-review-parser@latest
go install github.com/djdembeck/mira-pr-tools/cmd/mira-review-reply@latest
```

Make sure `$GOPATH/bin` is on your `$PATH`. Re-run those commands to update; there is no automatic update mechanism. See [GitHub Releases](https://github.com/djdembeck/mira-pr-tools/releases) for changelogs.

<details>
<summary>Pin a module install to a specific version</summary>

Replace `v1.0.0` with the tag you need:

```bash
go install github.com/djdembeck/mira-pr-tools/cmd/mira-review-parser@v1.0.0
go install github.com/djdembeck/mira-pr-tools/cmd/mira-review-reply@v1.0.0
```

</details>

## Usage

The examples below use the binaries produced by `make build`. If you used `go install`, invoke `mira-review-parser` and `mira-review-reply` without the `bin/` prefix. Run the commands from a checkout of the PR repository so the tools can inspect `git remote get-url origin`.

Parse open Mira findings as JSON (the default):

```bash
bin/mira-review-parser 123
```

The parser writes a `ParsedComment[]` JSON array to stdout and progress messages to stderr. It keeps open root comments from Mira authors by default.

Generate an alphabetically grouped consensus summary, include resolved threads, and include another reviewer login:

```bash
bin/mira-review-parser 123 \
  --format consensus \
  --include-resolved \
  --additional-authors nimuebot
```

Reject one false positive:

```bash
bin/mira-review-reply 123 \
  --reject 3564917980 \
  --reason "Auth at middleware layer"
```

Preview a batch reject without posting, with machine-readable results:

```bash
bin/mira-review-reply 123 \
  --batch-reject findings.json \
  --dry-run \
  --format json
```

The batch file is a JSON array of objects:

```json
[
  { "id": "3564917980", "reason": "Auth at middleware layer" }
]
```

Other reply actions:

```bash
# Acknowledge a valid finding and record the fixing commit.
bin/mira-review-reply 123 --acknowledge 3564917980 --commit abc123 --note "Covered by middleware tests"

# Let the tool find the active Mira bot from existing PR comments.
bin/mira-review-reply 123 --detect-bot
```

Use `mira-review-reply --help` for the complete reply option list. The available actions are `--reject`, `--acknowledge`, `--batch-reject`, `--batch-acknowledge`, and `--detect-bot`; common options include `--bot-name`, `--commit`, `--note`, `--dry-run`, and `--format json`.

Batch acknowledge files use the same array shape with `note` instead of `reason`:

```json
[
  { "id": "3564917980", "note": "Fixed in commit abc123" }
]
```

## Platform detection and configuration

Both binaries inspect `git remote get-url origin` and select a backend automatically:

- A remote containing `github.com` selects GitHub and uses the `gh` CLI.
- A remote containing `git.theiahd.nl` selects Forgejo and uses `FORGEJO_URL` plus `FORGEJO_TOKEN` for API access.
- Any other host defaults to GitHub.

For Forgejo review comments, replies preserve thread placement by using the review comment path and position when the API requires it. If a threaded endpoint is unavailable for a summary comment, the tool falls back to an issue comment.

## Output schema

`mira-review-parser` supports `--format json` (the default) and `--format consensus`. Consensus output is human-readable markdown grouped by file in alphabetical order. JSON output contains one object per parsed root comment; every field is present, with `null` used for optional values that were absent in the source comment.

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
  "suggestion": null,
  "agentPrompt": null,
  "diffHunk": null,
  "isResolved": false,
  "createdAt": "2026-07-11T...",
  "threadReplies": 0
}
```

The reply tool accepts `--format json` for reply results. Successful or failed results include the comment ID, action, generated body, and `success`; `error` and `replyUrl` are included when applicable.

## Building

`make build` builds both commands:

```bash
make build
```

The equivalent direct commands are:

```bash
go build -o bin/mira-review-parser ./cmd/mira-review-parser
go build -o bin/mira-review-reply ./cmd/mira-review-reply
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

Open an issue or pull request on [GitHub](https://github.com/djdembeck/mira-pr-tools). Keep changes focused, use [Conventional Commits](https://www.conventionalcommits.org/), and target the `main` branch; releases use semantic-version tags and are generated from commit messages.

Before submitting a change, run the formatting, vet, lint, build, and test commands in [Building](#building). Changes to Go dependencies must leave `go mod tidy` clean. CI also runs those checks on pull requests.

## License

MIT. See [LICENSE](LICENSE).

#!/usr/bin/env bash
# ci-changes.sh — classify changed files for selective CI gating.
#
# Environment:
#   CLASSIFY_EVENT  GitHub event name (pull_request, push, anything else)
#   CLASSIFY_BASE   base commit (PR base sha, or push before-sha)
#   CLASSIFY_HEAD   head commit (PR head sha, or push sha)
#   CLASSIFY_OUT    file to append key=value lines to
#                   (defaults to $GITHUB_OUTPUT when set)
#
# Appends to the output file:
#   workflow=true|false
#   lint=true|false
#   build=true|false
#   test=true|false
#
# Fail-safe direction: whenever the classification cannot be computed
# reliably (unrecognized event, missing pull_request base,
# missing/zero/unresolvable push base, merge-base failure, empty diff,
# git diff failure), all concerns are enabled and the script exits 0.
# A missing pull_request base is detected explicitly and warns; an
# unresolvable pull_request base is not pre-checked — merge-base fails,
# the original base is kept, the diff comes up empty, and the empty-diff
# catch-all at the end enables all concerns. An *unhandled* error (git
# unavailable, bad output file) aborts nonzero so the Changes job fails
# loudly.

# The inert path table below intentionally mirrors the original inline
# classifier verbatim; *.md already subsumes CHANGELOG.md and .github/*.md.
# shellcheck disable=SC2221,SC2222

set -uo pipefail

OUT="${CLASSIFY_OUT:-${GITHUB_OUTPUT-}}"
if [ -z "$OUT" ]; then
  echo "ci-changes: no output file (set CLASSIFY_OUT or GITHUB_OUTPUT)" >&2
  exit 1
fi

EVENT="${CLASSIFY_EVENT-}"
PUSH_ZERO_SHA="0000000000000000000000000000000000000000"

all_true() {
  {
    echo "workflow=true"
    echo "lint=true"
    echo "build=true"
    echo "test=true"
  } >> "$OUT"
}

BASE=""
HEAD=""

case "$EVENT" in
  pull_request)
    BASE="${CLASSIFY_BASE-}"
    HEAD="${CLASSIFY_HEAD-}"
    if [ -z "$BASE" ]; then
      echo "pull_request base SHA missing — enabling all concerns (fail-safe)"
      all_true
      exit 0
    fi
    # Diff against the merge base, not the base branch tip: while main
    # advances past the fork point, diffing base-tip..head would pick up
    # main's own commits. If merge-base cannot be computed, keep the
    # original base (old behavior, fail-safe direction).
    MB="$(git merge-base "$BASE" "$HEAD" 2>/dev/null || true)"
    if [ -n "$MB" ]; then
      BASE="$MB"
    fi
    ;;
  push)
    BASE="${CLASSIFY_BASE-}"
    HEAD="${CLASSIFY_HEAD-}"
    if [ -z "$BASE" ] || [ "$BASE" = "$PUSH_ZERO_SHA" ] || ! git rev-parse --verify -q "${BASE}^{commit}" >/dev/null 2>&1; then
      echo "before SHA missing, zero, or unresolvable — enabling all concerns (fail-safe)"
      all_true
      exit 0
    fi
    ;;
  *)
    # workflow_dispatch (and any unrecognized event): no diff available
    all_true
    exit 0
    ;;
esac

WORKFLOW=false
LINT=false
BUILD=false
TEST=false
SAW_PATH=0

while IFS= read -r -d '' path; do
  SAW_PATH=1
  case "$path" in
    .github/workflows/*.yml|.github/workflows/*.yaml) WORKFLOW=true ;;
    *_test.go) LINT=true; TEST=true ;;
    *.go) LINT=true; BUILD=true; TEST=true ;;
    go.mod|go.sum) LINT=true; BUILD=true; TEST=true ;;
    .golangci.yml) LINT=true ;;
    Makefile) BUILD=true ;;
    *.md|docs/*|renovate.json5|.pre-commit-config.yaml|.gitignore|.editorconfig|LICENSE|.env.example|.github/*.md|CHANGELOG.md|.release-please-manifest.json|release-please-config.json) : ;;
    *) LINT=true; BUILD=true; TEST=true ;;
  esac
done < <(git diff --name-only -z --no-renames "$BASE" "$HEAD" 2>/dev/null)

if [ "$SAW_PATH" -eq 0 ]; then
  echo "No diff paths (empty diff or classifier error) — enabling all concerns (fail-safe)"
  WORKFLOW=true
  LINT=true
  BUILD=true
  TEST=true
fi

{
  echo "workflow=$WORKFLOW"
  echo "lint=$LINT"
  echo "build=$BUILD"
  echo "test=$TEST"
} >> "$OUT"

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djdembeck/pr-review-tools/internal/review"
)

// Hooks export repository-local Git state; fixture subprocesses own another repo.
func fixtureProcessEnv() []string {
	env := os.Environ()
	clean := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, "GIT_") {
			clean = append(clean, entry)
		}
	}
	return clean
}

// initGitRepo creates a minimal git checkout with the given origin remote so
// the binary's platform detection and remote parsing run for real.
func initGitRepo(t *testing.T, dir, remote string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", remote},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = fixtureProcessEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// fakeGHDir writes an executable `gh` stub and returns the directory to put
// first on PATH. The stub answers the three lifecycle reads from environment
// variables so each test controls the served snapshot without a live API:
//
//	FAKE_GH_HEAD            head SHA for the snapshot query (required)
//	FAKE_GH_STATE           OPEN|CLOSED|MERGED (default OPEN)
//	FAKE_GH_REVIEWS         comma list of author=state=commit review nodes
//	FAKE_GH_ISSUE_COMMENTS  comma list of author:id top-level comments
//	FAKE_GH_FAIL            1 = every call exits non-zero
func fakeGHDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/bash
set -u
args="$*"
if [[ "${FAKE_GH_FAIL:-0}" == "1" ]]; then
  echo "error: api unavailable" >&2
  exit 1
fi
if [[ "$args" == *headRefOid* ]]; then
  printf '{"data":{"repository":{"pullRequest":{"headRefOid":"%s","state":"%s"}}}}' "${FAKE_GH_HEAD:-}" "${FAKE_GH_STATE:-OPEN}"
  exit 0
fi
if [[ "$args" == *"reviews(first"* ]]; then
  nodes=""
  IFS=',' read -ra items <<< "${FAKE_GH_REVIEWS:-}"
  for item in "${items[@]}"; do
    [[ -z "$item" ]] && continue
    IFS='=' read -r author state commit <<< "$item"
    [[ -n "$nodes" ]] && nodes="$nodes,"
    nodes="$nodes{\"id\":\"R-$author\",\"state\":\"$state\",\"submittedAt\":\"2026-01-01T00:00:00Z\",\"body\":\"\",\"url\":\"u\",\"commit\":{\"oid\":\"$commit\"},\"author\":{\"login\":\"$author\"}}"
  done
  printf '{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[%s],"pageInfo":{"hasNextPage":false,"endCursor":"c"}}}}}}' "$nodes"
  exit 0
fi
if [[ "$args" == *"reviewThreads(first"* ]]; then
  printf '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":"c"}}}}}}'
  exit 0
fi
if [[ "$args" == *"issues/"* && "$args" == *"/comments"* ]]; then
  nodes=""
  IFS=',' read -ra items <<< "${FAKE_GH_ISSUE_COMMENTS:-}"
  for item in "${items[@]}"; do
    [[ -z "$item" ]] && continue
    IFS=':' read -r author id <<< "$item"
    [[ -n "$nodes" ]] && nodes="$nodes,"
    nodes="$nodes{\"id\":$id,\"body\":\"summary\",\"created_at\":\"2026-01-01T00:00:02Z\",\"html_url\":\"u\",\"user\":{\"login\":\"$author\"}}"
  done
  printf '[[%s]]' "$nodes"
  exit 0
fi
echo "unexpected gh invocation: $args" >&2
exit 1
`
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}
	return dir
}

// fixtureEnv returns the process environment with the fake gh first on PATH.
func fixtureEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	binDir := fakeGHDir(t)
	env := append(fixtureProcessEnv(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	env = append(env, extra...)
	return env
}

// lifecycleFixtureConfig describes a state file seeded before a run.
type lifecycleFixtureConfig struct {
	state string
	head  string
}

// writeFixtureState seeds a valid, baseline-complete state file bound to the
// given head so resume/mismatch behavior can be driven through the real binary.
func writeFixtureState(t *testing.T, cfg lifecycleFixtureConfig) string {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := review.NewState(review.StateBinding{
		Platform: review.PlatformGitHub,
		Repo:     "owner/repo",
		PR:       7,
		Head:     cfg.head,
		WaitFor:  review.WaitForAll,
		Authors:  []string{"alice"},
	}, nil, now)
	st.BaselineComplete = true
	st.BaselineAt = now.Format(time.RFC3339Nano)
	payload, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture state: %v", err)
	}
	raw := string(payload) + "\n"
	if err := os.WriteFile(cfg.state, []byte(raw), 0o600); err != nil {
		t.Fatalf("write fixture state: %v", err)
	}
	return raw
}

// runWatchIn runs the binary with a working directory and environment.
func runWatchIn(t *testing.T, bin, dir string, env []string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	} else {
		cmd.Env = fixtureProcessEnv()
	}
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running watch %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return code, combined.String()
}

// exitCodeOf extracts the exit status, failing the test on a non-exec error.
func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("unexpected run error: %v", err)
	}
	return ee.ExitCode()
}

// extractLifecycleJSON returns the JSON object printed on stdout. The binary
// writes diagnostics to stderr and exactly one object to stdout, so the first
// '{' begins the object.
func extractLifecycleJSON(t *testing.T, out string) []byte {
	t.Helper()
	idx := strings.IndexByte(out, '{')
	if idx < 0 {
		t.Fatalf("no JSON object in output:\n%s", out)
	}
	var probe json.RawMessage
	dec := json.NewDecoder(strings.NewReader(out[idx:]))
	if err := dec.Decode(&probe); err != nil {
		t.Fatalf("stdout JSON is invalid: %v\noutput:\n%s", err, out)
	}
	return probe
}

// extractLifecycleStatus decodes just the status field.
func extractLifecycleStatus(t *testing.T, out string) string {
	t.Helper()
	var result review.LifecycleResult
	if err := json.Unmarshal(extractLifecycleJSON(t, out), &result); err != nil {
		t.Fatalf("decode lifecycle result: %v", err)
	}
	return result.Status
}

// readFile reads a file for assertions.
func readFile(path string) (string, error) {
	payload, err := os.ReadFile(path)
	return string(payload), err
}

// osStat exposes os.Stat for assertions.
func osStat(path string) (os.FileInfo, error) { return os.Stat(path) }

package main

import (
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/djdembeck/pr-review-tools/internal/review"
)

func TestLifecycleRejectsInvalidArguments(t *testing.T) {
	bin := buildWatchBinary(t)
	head := strings.Repeat("a", 40)
	cases := []struct {
		name string
		args []string
	}{
		{"bad wait-for", []string{"--wait-for", "most"}},
		{"missing state value", []string{"--state"}},
		{"missing head value", []string{"--head"}},
		{"missing repo value", []string{"--repo"}},
		{"timeout not a duration", []string{"--timeout", "abc"}},
		{"author flag-shaped", []string{"--authors", "--events"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			initGitRepo(t, dir, "git@github.com:owner/repo.git")
			state := filepath.Join(dir, "state.json")
			args := []string{"7", "--events", "--state", state, "--head", head,
				"--repo", "owner/repo", "--authors", "alice", "--timeout", "1s", "--poll", "10ms"}
			args = append(args, tt.args...)
			env := fixtureEnv(t, "FAKE_GH_HEAD="+head, "FAKE_GH_REVIEWS=alice=APPROVED="+head)
			code, out := runWatchIn(t, bin, dir, env, args...)
			if code != 1 {
				t.Fatalf("invalid invocation exit=%d, want1; output:\n%s", code, out)
			}
			if _, err := osStat(state); err == nil {
				t.Fatal("invalid invocation created a cursor")
			}
		})
	}
}

// TestResolveTargetFromRemote proves --repo overrides the receiving repo while
// the platform still comes from the origin host.
func TestResolveTargetFromRemote(t *testing.T) {
	cases := []struct {
		name     string
		remote   string
		override string
		owner    string
		repo     string
		platform review.Platform
		wantErr  bool
	}{
		{
			name: "github override", remote: "git@github.com:someone/else.git",
			override: "owner/repo", owner: "owner", repo: "repo", platform: review.PlatformGitHub,
		},
		{
			name: "forgejo override", remote: "https://git.theiahd.nl/someone/else.git",
			override: "owner/repo", owner: "owner", repo: "repo", platform: review.PlatformForgejo,
		},
		{
			name: "trailing slash normalized", remote: "git@github.com:x/y.git",
			override: "/owner/repo/", owner: "owner", repo: "repo", platform: review.PlatformGitHub,
		},
		{
			name: "missing override", remote: "git@github.com:x/y.git",
			wantErr: true,
		},
		{
			name: "bad override shape", remote: "git@github.com:x/y.git",
			override: "owner/repo/extra", wantErr: true,
		},
		{
			name: "empty segments", remote: "git@github.com:x/y.git",
			override: "/repo", wantErr: true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, platform, err := resolveTargetFromRemote(tt.remote, tt.override)
			if tt.wantErr {
				if err == nil {
					t.Fatal("invalid receiving repository was accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if owner != tt.owner || repo != tt.repo || platform != tt.platform {
				t.Fatalf("got %s/%s %s, want %s/%s %s", owner, repo, platform, tt.owner, tt.repo, tt.platform)
			}
		})
	}
}

// TestLifecycleRequiresRepoBeforeNetwork proves lifecycle mode fails closed
// without --repo rather than silently using origin.
func TestLifecycleRequiresRepoBeforeNetwork(t *testing.T) {
	bin := buildWatchBinary(t)
	dir := t.TempDir()
	initGitRepo(t, dir, "git@github.com:owner/repo.git")
	state := filepath.Join(dir, "state.json")
	head := strings.Repeat("a", 40)

	cmd := exec.Command(bin, "7", "--events", "--state", state, "--head", head, "--authors", "alice", "--timeout", "1s", "--poll", "10ms")
	cmd.Dir = dir
	cmd.Env = fixtureEnv(t, "FAKE_GH_HEAD="+head, "FAKE_GH_REVIEWS=alice=APPROVED="+head)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running watch: %v", err)
		}
		code = ee.ExitCode()
	}
	if code != 1 {
		t.Fatalf("exit = %d, want 1; output:\n%s", code, out)
	}
	// No state file may be created when setup fails.
	if _, statErr := osStat(state); statErr == nil {
		t.Fatalf("setup failure must not create a state file")
	}
}

// TestLifecycleStateMismatchExitsOne drives the real binary through a resumable
// round: a state file bound to another head must exit 1 and print status
// "error" without rewriting the file.
func TestLifecycleStateMismatchExitsOne(t *testing.T) {
	bin := buildWatchBinary(t)
	repoDir := t.TempDir()
	initGitRepo(t, repoDir, "git@github.com:owner/repo.git")
	state := filepath.Join(repoDir, "state.json")

	cfg := lifecycleFixtureConfig{
		state: state,
		head:  strings.Repeat("a", 40),
	}
	seeded := writeFixtureState(t, cfg)

	cmd := exec.Command(bin, "7", "--events", "--state", state,
		"--head", strings.Repeat("b", 40), "--repo", "owner/repo",
		"--authors", "alice", "--timeout", "1s", "--poll", "1s")
	cmd.Dir = repoDir
	cmd.Env = fixtureEnv(t)
	out, err := cmd.CombinedOutput()
	code := exitCodeOf(t, err)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; output:\n%s", code, out)
	}

	status := extractLifecycleStatus(t, string(out))
	if status != review.StatusError {
		t.Fatalf("status = %q, want %q", status, review.StatusError)
	}

	// The mismatched state file must be preserved verbatim.
	after, readErr := readFile(state)
	if readErr != nil {
		t.Fatalf("state must still exist: %v", readErr)
	}
	if after != seeded {
		t.Fatalf("state file must not be rewritten on a mismatch")
	}
}

func TestLifecycleAPIFailureNeverBecomesReviewTimeout(t *testing.T) {
	bin := buildWatchBinary(t)
	repoDir := t.TempDir()
	initGitRepo(t, repoDir, "git@github.com:owner/repo.git")
	state := filepath.Join(repoDir, "state.json")

	cmd := exec.Command(bin, "7", "--events", "--state", state,
		"--head", strings.Repeat("a", 40), "--repo", "owner/repo",
		"--authors", "alice", "--timeout", "2s", "--poll", "1s")
	cmd.Dir = repoDir
	cmd.Env = fixtureEnv(t)
	out, err := cmd.CombinedOutput()
	code := exitCodeOf(t, err)

	var result review.LifecycleResult
	if jsonErr := json.Unmarshal(extractLifecycleJSON(t, string(out)), &result); jsonErr != nil {
		t.Fatalf("stdout must be a lifecycle JSON object: %v\noutput:\n%s", jsonErr, out)
	}
	if code != 1 || result.Status != review.StatusError {
		t.Fatalf("exit=%d status=%q, want 1/error (no successful PR observation)", code, result.Status)
	}
}

// TestLegacyModeUnaffectedByLifecycleFlags proves legacy invocations still fail
// at the git/self stage without lifecycle output, so the new mode cannot leak
// into the old one.
func TestLegacyModeUnaffectedByLifecycleFlags(t *testing.T) {
	bin := buildWatchBinary(t)

	// Positional validation stays first, even with --events.
	code, out := runWatch(t, bin, "", "abc", "--events")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(out, "Invalid PR number") {
		t.Fatalf("positional validation must stay first:\n%s", out)
	}

	// A legacy invocation outside any git checkout fails at the remote step
	// (exit 1) and must never print lifecycle JSON.
	dir := t.TempDir()
	code, out = runWatchIn(t, bin, dir, nil, "42", "--timeout", "1s")
	if code != 1 {
		t.Fatalf("legacy run outside a git repo must exit 1, got %d:\n%s", code, out)
	}
	if strings.Contains(out, `"statePath"`) || strings.Contains(out, "review-watch: armed") {
		t.Fatalf("legacy mode must not emit lifecycle output:\n%s", out)
	}
	if !strings.Contains(out, "git remote get-url") && !strings.Contains(out, "Error") {
		t.Fatalf("legacy failure must be reported:\n%s", out)
	}
}

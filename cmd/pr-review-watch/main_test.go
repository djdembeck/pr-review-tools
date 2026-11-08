package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djdembeck/pr-review-tools/internal/review"
)

// TestWatchFindsNewComment verifies the success path: a comment whose ID
// was not in the baseline is returned in fetch order.
func TestWatchFindsNewComment(t *testing.T) {
	calls := 0
	fetch := func(ctx context.Context) ([]review.ParsedComment, error) {
		calls++
		if calls == 1 {
			return []review.ParsedComment{{ID: "a"}}, nil
		}
		return []review.ParsedComment{{ID: "a"}, {ID: "b"}}, nil
	}
	fresh, polls, waited, err := watch(context.Background(), []string{"a"}, 2*time.Second, 10*time.Millisecond, fetch)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if len(fresh) != 1 || fresh[0].ID != "b" {
		t.Fatalf("expected exactly the new comment b, got %+v", fresh)
	}
	if polls < 1 {
		t.Fatalf("expected at least one poll, got %d", polls)
	}
	if waited >= 2*time.Second {
		t.Fatalf("expected to finish before the timeout, waited %s", waited)
	}
}

// TestWatchTimesOut verifies the timeout path: no new comment before the
// deadline yields a timeout error and no comments.
func TestWatchTimesOut(t *testing.T) {
	fetch := func(ctx context.Context) ([]review.ParsedComment, error) {
		return []review.ParsedComment{{ID: "a"}}, nil
	}
	fresh, _, _, err := watch(context.Background(), []string{"a"}, 100*time.Millisecond, 10*time.Millisecond, fetch)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !errors.Is(err, errWatchTimeout) {
		t.Fatalf("expected errWatchTimeout, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no new review comments") {
		t.Fatalf("timeout error must mention no new review comments, got: %v", err)
	}
	if fresh != nil {
		t.Fatalf("expected nil comments on timeout, got %+v", fresh)
	}
}

// TestWatchIgnoresTransientErrors verifies that transient fetch failures
// during polling are tolerated: the watch keeps going and still succeeds
// once a new comment appears.
func TestWatchIgnoresTransientErrors(t *testing.T) {
	calls := 0
	fetch := func(ctx context.Context) ([]review.ParsedComment, error) {
		calls++
		switch calls {
		case 1:
			return []review.ParsedComment{{ID: "a"}}, nil
		case 2, 3:
			return nil, errors.New("network blip")
		default:
			return []review.ParsedComment{{ID: "a"}, {ID: "b"}}, nil
		}
	}
	fresh, _, _, err := watch(context.Background(), []string{"a"}, 2*time.Second, 10*time.Millisecond, fetch)
	if err != nil {
		t.Fatalf("expected success after transient errors, got: %v", err)
	}
	if len(fresh) != 1 || fresh[0].ID != "b" {
		t.Fatalf("expected exactly the new comment b, got %+v", fresh)
	}
}

// TestWatchPersistentFetchFailure verifies that a persistent fetch failure
// (>= watchFetchFailureThreshold consecutive failed polls) stops the watch
// with a fetch-failure error — the exit-1 path — instead of running to the
// deadline and reporting a timeout (exit 2).
func TestWatchPersistentFetchFailure(t *testing.T) {
	calls := 0
	fetch := func(ctx context.Context) ([]review.ParsedComment, error) {
		calls++
		return nil, errors.New("401: token expired")
	}
	_, polls, _, err := watch(context.Background(), []string{"a"}, 5*time.Second, 10*time.Millisecond, fetch)
	if err == nil {
		t.Fatal("expected a fetch-failure error, got nil")
	}
	if !errors.Is(err, errWatchFetchFailure) {
		t.Fatalf("expected errWatchFetchFailure, got: %v", err)
	}
	if errors.Is(err, errWatchTimeout) {
		t.Fatalf("persistent failure must not be reported as a timeout, got: %v", err)
	}
	if polls != 0 {
		t.Fatalf("no poll may succeed while every fetch fails, got %d polls", polls)
	}
	if calls < watchFetchFailureThreshold {
		t.Fatalf("expected at least %d failed polls, got %d", watchFetchFailureThreshold, calls)
	}
}

// TestWatchBlockedFetchHonorsDeadline verifies the deadline is enforced
// DURING a fetch: a fetch that blocks until its context is cancelled must
// yield the timeout error well before the full timeout elapses, not keep
// the watch alive past the deadline.
func TestWatchBlockedFetchHonorsDeadline(t *testing.T) {
	timeout := 400 * time.Millisecond
	fetch := func(ctx context.Context) ([]review.ParsedComment, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	_, _, _, err := watch(context.Background(), []string{"a"}, timeout, 100*time.Millisecond, fetch)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !errors.Is(err, errWatchTimeout) {
		t.Fatalf("expected errWatchTimeout, got: %v", err)
	}
	if elapsed < timeout-50*time.Millisecond {
		t.Fatalf("blocked fetch must be cancelled near the deadline, finished after %s (< %s): the deadline was not enforced during fetching", elapsed, timeout)
	}
}

// TestWatchLateFetchAfterDeadline verifies the deadline is enforced during
// fetching: a slow poll is cancelled at the deadline (a fetch error, so the
// timeout path is reached without a persistent-failure threshold), and a
// poll that returns fresh comments after the deadline must NOT be reported
// as success.
func TestWatchLateFetchAfterDeadline(t *testing.T) {
	timeout := 150 * time.Millisecond

	t.Run("slow poll cancelled at deadline", func(t *testing.T) {
		fetch := func(ctx context.Context) ([]review.ParsedComment, error) {
			// A real backend (exec.CommandContext / NewRequestWithContext)
			// aborts on ctx cancellation and returns an error.
			select {
			case <-time.After(250 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return nil, nil
		}
		_, _, _, err := watch(context.Background(), []string{"a"}, timeout, 50*time.Millisecond, fetch)
		if err == nil {
			t.Fatal("expected a timeout error for a cancelled poll, got nil")
		}
		if !errors.Is(err, errWatchTimeout) {
			t.Fatalf("cancelled poll must be reported as a timeout, got: %v", err)
		}
	})

	t.Run("late success is not success", func(t *testing.T) {
		fetch := func(context.Context) ([]review.ParsedComment, error) {
			// Defensive: a result that lands after the deadline must not be
			// reported as success.
			time.Sleep(250 * time.Millisecond)
			return []review.ParsedComment{{ID: "a"}, {ID: "late-b"}}, nil
		}
		_, _, _, err := watch(context.Background(), []string{"a"}, timeout, 50*time.Millisecond, fetch)
		if err == nil {
			t.Fatal("a poll landing after the deadline must not be reported as success")
		}
		if !errors.Is(err, errWatchTimeout) {
			t.Fatalf("late success must be reported as a timeout, got: %v", err)
		}
	})
}

// TestWatchIgnoresReopenedBaselineThread verifies the baseline contract
// through the shared pipeline: a root that is resolved at start (hence
// absent from the filtered polling set) is still captured in the pre-filter
// baseline, so reopening it later does NOT trigger success.
func TestWatchIgnoresReopenedBaselineThread(t *testing.T) {
	// The baseline capture runs RelevantComments with baselineOut, exactly
	// like main(): resolved root "a" is dropped from Comments but its ID is
	// recorded in the baseline.
	rawStart := []review.RawComment{
		{ID: "a", Author: "alice", IsResolved: true},
		{ID: "b", Author: "bob"},
	}
	baselineIDs := make([]string, 0)
	base := review.RelevantComments(rawStart, review.RelevantOptions{}, &baselineIDs)
	if len(base.Comments) != 1 || base.Comments[0].ID != "b" {
		t.Fatalf("baseline filter must drop resolved root a, got %+v", base.Comments)
	}
	got := map[string]bool{}
	for _, id := range baselineIDs {
		got[id] = true
	}
	if !got["a"] || !got["b"] {
		t.Fatalf("baseline must capture pre-filter roots a and b, got %v", baselineIDs)
	}

	// The poll later returns the thread with the resolved flag cleared.
	fetch := func(ctx context.Context) ([]review.ParsedComment, error) {
		res := review.RelevantComments([]review.RawComment{
			{ID: "a", Author: "alice"},
			{ID: "b", Author: "bob"},
		}, review.RelevantOptions{}, nil)
		return res.Comments, nil
	}
	fresh, _, _, err := watch(context.Background(), baselineIDs, 200*time.Millisecond, 10*time.Millisecond, fetch)
	if err == nil {
		t.Fatalf("reopened baseline thread must not trigger success, got comments %+v", fresh)
	}
	if !errors.Is(err, errWatchTimeout) {
		t.Fatalf("expected a timeout, got: %v", err)
	}
	if fresh != nil {
		t.Fatalf("expected no comments, got %+v", fresh)
	}
}

// TestWatchBaselineIsFatal proves the baseline-diff semantics with a zero
// baseline: the first poll's comments are all "new" and immediately trigger
// success. (The baseline fetch itself lives in main()'s closure contract,
// which is out of reach in-process.)
func TestWatchBaselineIsFatal(t *testing.T) {
	fetch := func(ctx context.Context) ([]review.ParsedComment, error) {
		return []review.ParsedComment{{ID: "a"}}, nil
	}
	fresh, _, _, err := watch(context.Background(), nil, 2*time.Second, 10*time.Millisecond, fetch)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if len(fresh) != 1 || fresh[0].ID != "a" {
		t.Fatalf("expected the zero-baseline comment to be reported new, got %+v", fresh)
	}
}

// TestParseHead verifies the positional PR-number / help seam: --help and -h
// MUST short-circuit to help regardless of position or PR-number validity,
// while non-help invocations with an invalid PR number still error.
func TestParseHead(t *testing.T) {
	t.Run("help first arg, no PR number", func(t *testing.T) {
		for _, flag := range []string{"--help", "-h"} {
			pr, showHelp, err := parseHead([]string{flag})
			if !showHelp || pr != 0 || err != "" {
				t.Fatalf("parseHead([%s]) = %d, %v, %q; want help requested", flag, pr, showHelp, err)
			}
		}
	})

	t.Run("help with valid or invalid PR number", func(t *testing.T) {
		cases := [][]string{{"42", "--help"}, {"--help", "42"}, {"abc", "-h"}, {"-h", "--timeout", "5s"}}
		for _, args := range cases {
			if pr, showHelp, err := parseHead(args); !showHelp || pr != 0 || err != "" {
				t.Fatalf("parseHead(%v) = %d, %v, %q; want help requested", args, pr, showHelp, err)
			}
		}
	})

	t.Run("valid PR number", func(t *testing.T) {
		pr, showHelp, err := parseHead([]string{"42", "--timeout", "5s"})
		if showHelp || err != "" || pr != 42 {
			t.Fatalf("parseHead([42 --timeout 5s]) = %d, %v, %q; want prNumber 42, no help, no error", pr, showHelp, err)
		}
	})

	t.Run("invalid PR number still errors", func(t *testing.T) {
		cases := []struct {
			args []string
			want string
		}{
			{[]string{"abc"}, "Invalid PR number: abc"},
			{[]string{"0"}, "Invalid PR number: 0"},
			{[]string{"-3"}, "Invalid PR number: -3"},
		}
		for _, tt := range cases {
			pr, showHelp, err := parseHead(tt.args)
			if showHelp || pr != 0 {
				t.Fatalf("parseHead(%v) = %d, %v; want error, no help", tt.args, pr, showHelp)
			}
			if err != tt.want {
				t.Fatalf("parseHead(%v) error = %q, want %q", tt.args, err, tt.want)
			}
		}
	})
}

// TestUsageTextComplete verifies the banner printed for --help/-h and no args
// actually lists the documented flags, so the help cannot regress to a
// signpost-only message.
func TestUsageTextComplete(t *testing.T) {
	for _, want := range []string{
		"--timeout <duration>",
		"--poll <duration>",
		"--authors <csv>",
		"--trusted-authors <csv>",
		"--include-self",
		"--format json|consensus",
		"--help, -h",
	} {
		if !strings.Contains(usageText, want) {
			t.Errorf("usageText must document %q", want)
		}
	}
}

// TestParseCSVLogins verifies CSV splitting: trimming, empty-entry removal,
// and flag-shaped token rejection (caught at flag-parsing time, before any
// API call, so no os.Exit is reachable in-process for this shape).
func TestParseCSVLogins(t *testing.T) {
	got := parseCSVLogins("trusted-authors", " nimuebot , copilot ,,")
	want := []string{"nimuebot", "copilot"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	if got := parseCSVLogins("authors", "   , , "); len(got) != 0 {
		t.Fatalf("all-empty CSV must yield no logins, got %v", got)
	}

	cases := []struct {
		name string
		val  string
		want []string
	}{
		{"valid single login", "alice", []string{"alice"}},
		{"bare dash accepted", "-", []string{"-"}},
		{"empty string skipped", "", nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCSVLogins("authors", tt.val)
			if len(got) != len(tt.want) {
				t.Fatalf("parseCSVLogins(\"authors\", %q) = %v, want %v", tt.val, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("parseCSVLogins(\"authors\", %q) = %v, want %v", tt.val, got, tt.want)
				}
			}
		})
	}
}

// buildWatchBinary builds the watch binary once into a per-test temp dir and
// returns the binary path. Subprocess tests need it because the flag-parsing
// branches under test live inline in main() and os.Exit on the error path.
func buildWatchBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pr-review-watch")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building pr-review-watch: %v\n%s", err, out)
	}
	return bin
}

// runWatch runs the binary with the given args and reports exit status and
// combined stdout+stderr. When cwd is non-empty the subprocess runs there —
// setup-failure cases use an empty directory with no git repository so the
// git/fetch stage fails deterministically without live network access.
// gitEnvFilter drops GIT_* variables so the subprocess cannot inherit a
// git repository from the caller's environment (the pre-commit hook and
// `go test` run with GIT_DIR/GIT_WORK_TREE set). This keeps the cwd-based
// "no repository" cases deterministic regardless of how the suite is run.
func gitEnvFilter() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); strings.HasPrefix(k, "GIT_") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func runWatch(t *testing.T, bin, cwd string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = gitEnvFilter()
	if cwd != "" {
		cmd.Dir = cwd
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running watch %v: %v", args, err)
		}
	}
	return code, string(out)
}

// TestFlagParseSubprocess drives the inline flag-parsing branches in main()
// through the real binary: invalid durations, flag-shaped author values, and
// poll >= timeout are rejected before any API call. The valid-args case runs
// the binary from an empty temp dir (no git repository, no origin remote),
// so it fails deterministically at the git-remote setup stage with
// exit 1 — no live network access and no environment dependence.
func TestFlagParseSubprocess(t *testing.T) {
	bin := buildWatchBinary(t)
	emptyDir := t.TempDir() // no .git, no origin remote

	cases := []struct {
		name          string
		args          []string
		cwd           string
		wantCode      int
		wantInOutput  string // substring that MUST appear
		wantNotOutput string // substring that MUST NOT appear
	}{
		{
			name:         "timeout non-duration rejected",
			args:         []string{"42", "--timeout", "abc"},
			wantCode:     1,
			wantInOutput: "--timeout must be a positive duration",
		},
		{
			name:         "timeout zero rejected",
			args:         []string{"42", "--timeout", "0s"},
			wantCode:     1,
			wantInOutput: "--timeout must be a positive duration",
		},
		{
			name:         "poll negative rejected",
			args:         []string{"42", "--poll", "-5s"},
			wantCode:     1,
			wantInOutput: "--poll must be a positive duration",
		},
		{
			name:         "authors flag-shaped value rejected",
			args:         []string{"42", "--authors", "--include-self"},
			wantCode:     1,
			wantInOutput: "authors got flag-shaped value",
		},
		{
			name:         "authors empty CSV requires non-empty value",
			args:         []string{"42", "--authors", ""},
			wantCode:     1,
			wantInOutput: "Error: --authors requires a non-empty value",
		},
		{
			name:         "poll equal to timeout rejected",
			args:         []string{"42", "--poll", "10s", "--timeout", "10s"},
			wantCode:     1,
			wantInOutput: "--poll",
		},
		{
			name:         "poll greater than timeout rejected",
			args:         []string{"42", "--poll", "30s", "--timeout", "20s"},
			wantCode:     1,
			wantInOutput: "must be less than --timeout",
		},
		{
			// Flag parsing succeeds; in a directory with no git repository
			// the run MUST fail at the git-remote setup stage (exit 1) —
			// deterministic, no network, and specific to the setup-failure
			// path.
			name:          "valid args fail at git remote stage",
			args:          []string{"99999", "--timeout", "5s", "--poll", "1s"},
			cwd:           emptyDir,
			wantCode:      1,
			wantInOutput:  "git remote",
			wantNotOutput: "flag-shaped",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			code, out := runWatch(t, bin, tt.cwd, tt.args...)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d; output:\n%s", code, tt.wantCode, out)
			}
			if tt.wantInOutput != "" && !strings.Contains(out, tt.wantInOutput) {
				t.Fatalf("output must contain %q:\n%s", tt.wantInOutput, out)
			}
			if tt.wantNotOutput != "" && strings.Contains(out, tt.wantNotOutput) {
				t.Fatalf("output must not contain %q:\n%s", tt.wantNotOutput, out)
			}
		})
	}
}

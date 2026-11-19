package review

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestRunGhWithTimeoutRoundDeadlineBranch proves the abort mapping in
// runGhWithTimeout: a parent whose round deadline has already expired maps
// the failure to the round sentinel, while a parent canceled for another
// reason stays an ordinary fetch failure. Both cases fail before the gh
// binary is resolved, so the test needs no gh on PATH.
func TestRunGhWithTimeoutRoundDeadlineBranch(t *testing.T) {
	cases := []struct {
		name         string
		parent       func(t *testing.T) context.Context
		wantSentinel bool
		wantText     string
	}{
		{
			name: "expired round deadline is a round abort",
			parent: func(t *testing.T) context.Context {
				ctx, cancel := context.WithTimeout(context.Background(), -time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			},
			wantSentinel: true,
			wantText:     "aborted by round deadline",
		},
		{
			name: "canceled parent is an ordinary fetch failure",
			parent: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantSentinel: false,
			wantText:     "gh request canceled",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			// An empty PATH: the deadline branch must fire before the CLI is
			// ever resolved, so no gh binary is required.
			t.Setenv("PATH", t.TempDir())
			_, err := runGhWithTimeout(tt.parent(t), []string{"api", "graphql"}, 30*time.Second)
			if err == nil {
				t.Fatal("an aborted round request must fail")
			}
			if got := errors.Is(err, errRoundDeadline); got != tt.wantSentinel {
				t.Fatalf("errors.Is(err, errRoundDeadline) = %v, want %v (err=%v)", got, tt.wantSentinel, err)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("error = %q, want it to contain %q", err, tt.wantText)
			}
		})
	}
}

// TestGitHubBackendRunPreflightRoundDeadline proves the preflight in
// githubEventBackend.run: an already-expired round context is reported with
// the round sentinel before any gh invocation happens.
func TestGitHubBackendRunPreflightRoundDeadline(t *testing.T) {
	calls := 0
	backend := githubEventBackend{
		owner: "o", repo: "r", pr: 1,
		runGHFn: func(context.Context, []string, time.Duration) (string, error) {
			calls++
			return "must not run", nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), -time.Millisecond)
	defer cancel()
	_, err := backend.run(ctx, []string{"api", "graphql"})
	if calls != 0 {
		t.Fatalf("preflight fired, but the CLI seam was still called %d time(s)", calls)
	}
	if !errors.Is(err, errRoundDeadline) {
		t.Fatalf("error = %v, want the round deadline sentinel", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want it to carry context.DeadlineExceeded", err)
	}
}

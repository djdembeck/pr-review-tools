package review

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestForgejoBackendGetPreflightRoundDeadline proves the preflight in
// forgejoEventBackend.get: an already-expired round context is reported with
// the round sentinel before any HTTP request happens, carrying the original
// context error.
func TestForgejoBackendGetPreflightRoundDeadline(t *testing.T) {
	calls := 0
	backend := forgejoEventBackend{
		owner: "o", repo: "r", pr: 1,
		getFn: func(context.Context, string, time.Duration) (string, error) {
			calls++
			return "must not fetch", nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), -time.Millisecond)
	defer cancel()
	_, err := backend.get(ctx, "repos/o/r/pulls/1")
	if calls != 0 {
		t.Fatalf("preflight fired, but the HTTP seam was still called %d time(s)", calls)
	}
	if !errors.Is(err, errRoundDeadline) {
		t.Fatalf("error = %v, want the round deadline sentinel", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want it to carry context.DeadlineExceeded", err)
	}
}

// TestForgejoBackendGetRoundAbortWrap proves the post-request mapping in
// forgejoEventBackend.get: an in-flight round abort is tagged with the round
// sentinel, while a request-local failure on an alive round stays an ordinary
// fetch error.
func TestForgejoBackendGetRoundAbortWrap(t *testing.T) {
	// The abort is triggered from inside the seam so the post-request branch
	// (not the preflight) is what wraps the fetch error with the sentinel.
	t.Run("round abort mid-request is a round deadline", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		backend := forgejoEventBackend{
			owner: "o", repo: "r", pr: 1,
			getFn: func(ctx context.Context, _ string, _ time.Duration) (string, error) {
				// Simulate the round deadline expiring while the request is
				// in flight: the context ends and the fetch reports it.
				cancel()
				return "", ctx.Err()
			},
		}
		_, err := backend.get(ctx, "repos/o/r/pulls/1")
		if !errors.Is(err, errRoundDeadline) {
			t.Fatalf("error = %v, want the round deadline sentinel", err)
		}
	})
	t.Run("request-local failure on an alive round is a fetch error", func(t *testing.T) {
		failure := errors.New("request timeout")
		backend := forgejoEventBackend{
			owner: "o", repo: "r", pr: 1,
			getFn: func(context.Context, string, time.Duration) (string, error) {
				return "", failure
			},
		}
		_, err := backend.get(context.Background(), "repos/o/r/pulls/1")
		if !errors.Is(err, failure) {
			t.Fatalf("error = %v, want the request-local failure", err)
		}
		if errors.Is(err, errRoundDeadline) {
			t.Fatalf("a request-local failure must not carry the round deadline sentinel: %v", err)
		}
	})
}

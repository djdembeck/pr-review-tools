package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/djdembeck/pr-review-tools/internal/review"
)

const e2eHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// e2eFixture prepares a real git checkout plus a fake gh on PATH.
type e2eFixture struct {
	bin     string
	repoDir string
	state   string
	env     []string
}

func newE2EFixture(t *testing.T, env ...string) e2eFixture {
	t.Helper()
	bin := buildWatchBinary(t)
	repoDir := t.TempDir()
	initGitRepo(t, repoDir, "git@github.com:owner/repo.git")
	return e2eFixture{
		bin:     bin,
		repoDir: repoDir,
		state:   filepath.Join(repoDir, "state.json"),
		env:     fixtureEnv(t, env...),
	}
}

// lifecycleArgs builds the canonical lifecycle invocation.
func (f e2eFixture) args(head string, extra ...string) []string {
	args := []string{
		"7", "--events", "--state", f.state, "--head", head,
		"--repo", "owner/repo", "--authors", "alice,bob",
		"--timeout", "1500ms", "--poll", "200ms",
	}
	return append(args, extra...)
}

// decodeLifecycle decodes the printed status object.
func decodeLifecycle(t *testing.T, out string) review.LifecycleResult {
	t.Helper()
	var result review.LifecycleResult
	if err := json.Unmarshal(extractLifecycleJSON(t, out), &result); err != nil {
		t.Fatalf("decode lifecycle result: %v\noutput:\n%s", err, out)
	}
	return result
}

// TestE2EReadyOnCurrentHeadReviews is the happy path through the real binary:
// both selected authors have reviews bound to the head.
func TestE2EReadyOnCurrentHeadReviews(t *testing.T) {
	f := newE2EFixture(t,
		"FAKE_GH_HEAD="+e2eHead,
		"FAKE_GH_REVIEWS=alice=APPROVED="+e2eHead+",bob=COMMENTED="+e2eHead,
	)
	code, out := runWatchIn(t, f.bin, f.repoDir, f.env, f.args(e2eHead)...)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; output:\n%s", code, out)
	}
	result := decodeLifecycle(t, out)
	if result.Status != review.StatusReady {
		t.Fatalf("status = %q, want ready", result.Status)
	}
	if len(result.Responded) != 2 || len(result.Outstanding) != 0 {
		t.Fatalf("responded=%v outstanding=%v", result.Responded, result.Outstanding)
	}

	// The armed marker must appear only after the baseline is durable.
	if !strings.Contains(out, "review-watch: armed") {
		t.Fatalf("output must carry the armed marker:\n%s", out)
	}
	st, ok, err := review.LoadState(f.state)
	if err != nil || !ok {
		t.Fatalf("state must be durable after the round: ok=%v err=%v", ok, err)
	}
	if st.Head != e2eHead || st.PR != 7 || !st.BaselineComplete {
		t.Fatalf("state binding = %+v", st)
	}
	info, statErr := os.Stat(f.state)
	if statErr != nil {
		t.Fatalf("stat state: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state mode = %04o, want 0600", perm)
	}
}

// TestE2ETimeoutPersistsProgress proves a timed-out round exits 2 and leaves a
// resumable cursor rather than discarding collected evidence.
func TestE2ETimeoutPersistsProgress(t *testing.T) {
	f := newE2EFixture(t,
		"FAKE_GH_HEAD="+e2eHead,
		"FAKE_GH_REVIEWS=alice=APPROVED="+e2eHead,
	)
	code, out := runWatchIn(t, f.bin, f.repoDir, f.env, f.args(e2eHead)...)
	if code != 2 {
		t.Fatalf("exit = %d, want 2; output:\n%s", code, out)
	}
	result := decodeLifecycle(t, out)
	if result.Status != review.StatusTimeout {
		t.Fatalf("status = %q, want timeout", result.Status)
	}
	if len(result.Responded) != 1 {
		t.Fatalf("responded = %v, want only alice", result.Responded)
	}
	if len(result.Outstanding) != 1 || !strings.EqualFold(result.Outstanding[0], "bob") {
		t.Fatalf("outstanding = %v, want only bob", result.Outstanding)
	}
	if _, err := os.Stat(f.state); err != nil {
		t.Fatalf("timed-out round must persist state: %v", err)
	}
}

// TestE2EResumeCreditsGapEvents is the CI-gap regression test at the CLI level:
// round 1 times out with nothing, bob's review then lands while no watcher is
// running, and round 2 must credit it without rebaselining it away.
func TestE2EResumeCreditsGapEvents(t *testing.T) {
	f := newE2EFixture(t, "FAKE_GH_HEAD="+e2eHead, "FAKE_GH_REVIEWS=alice=APPROVED="+e2eHead)
	code, out := runWatchIn(t, f.bin, f.repoDir, f.env, f.args(e2eHead)...)
	if code != 2 {
		t.Fatalf("round 1 exit = %d, want 2; output:\n%s", code, out)
	}
	if got := decodeLifecycle(t, out).Polls; got == 0 {
		t.Fatalf("round 1 must have polled at least once")
	}

	// The gap: bob responds. Round 2 resumes the same state path.
	f.env = fixtureEnv(t,
		"FAKE_GH_HEAD="+e2eHead,
		"FAKE_GH_REVIEWS=alice=APPROVED="+e2eHead+",bob=APPROVED="+e2eHead,
	)
	code, out = runWatchIn(t, f.bin, f.repoDir, f.env, f.args(e2eHead)...)
	if code != 0 {
		t.Fatalf("round 2 exit = %d, want 0; output:\n%s", code, out)
	}
	result := decodeLifecycle(t, out)
	if result.Status != review.StatusReady {
		t.Fatalf("round 2 status = %q, want ready (outstanding=%v)", result.Status, result.Outstanding)
	}
	if !strings.Contains(out, "Resuming state") {
		t.Fatalf("round 2 must log a resume:\n%s", out)
	}
}

// TestE2EIssueSummaryOnlyCounts covers the summary-only reviewer path: no
// review object, just a top-level comment arriving after the baseline.
func TestE2EIssueSummaryOnlyCounts(t *testing.T) {
	f := newE2EFixture(t, "FAKE_GH_HEAD="+e2eHead, "FAKE_GH_REVIEWS=alice=APPROVED="+e2eHead)
	// Round 1 arms a baseline with no issue comments.
	code, out := runWatchIn(t, f.bin, f.repoDir, f.env, f.args(e2eHead)...)
	if code != 2 {
		t.Fatalf("round 1 exit = %d, want 2; output:\n%s", code, out)
	}

	// Bob publishes a summary comment during the gap.
	f.env = fixtureEnv(t,
		"FAKE_GH_HEAD="+e2eHead,
		"FAKE_GH_REVIEWS=alice=APPROVED="+e2eHead,
		"FAKE_GH_ISSUE_COMMENTS=bob:42",
	)
	code, out = runWatchIn(t, f.bin, f.repoDir, f.env, f.args(e2eHead)...)
	if code != 0 {
		t.Fatalf("round 2 exit = %d, want 0; output:\n%s", code, out)
	}
	result := decodeLifecycle(t, out)
	if result.Status != review.StatusReady {
		t.Fatalf("status = %q, want ready for a summary-only response", result.Status)
	}
	var stable bool
	for _, e := range result.Events {
		if e.Kind == review.EventKindIssueComment && strings.EqualFold(e.Author, "bob") {
			if e.Binding != review.BindingStableHead {
				t.Fatalf("summary binding = %q, want %q (never commit-bound)", e.Binding, review.BindingStableHead)
			}
			stable = true
		}
	}
	if !stable {
		t.Fatalf("expected bob's summary event, got %+v", result.Events)
	}
}

// TestE2EPreExistingSummaryDoesNotCount proves an old top-level comment cannot
// complete an author for a fresh round.
func TestE2EPreExistingSummaryDoesNotCount(t *testing.T) {
	f := newE2EFixture(t,
		"FAKE_GH_HEAD="+e2eHead,
		"FAKE_GH_REVIEWS=alice=APPROVED="+e2eHead,
		"FAKE_GH_ISSUE_COMMENTS=bob:42",
	)
	code, out := runWatchIn(t, f.bin, f.repoDir, f.env, f.args(e2eHead)...)
	if code != 2 {
		t.Fatalf("exit = %d, want 2; output:\n%s", code, out)
	}
	result := decodeLifecycle(t, out)
	if len(result.Outstanding) != 1 || !strings.EqualFold(result.Outstanding[0], "bob") {
		t.Fatalf("outstanding = %v, want bob: a pre-existing summary must not complete", result.Outstanding)
	}
}

// TestE2EHeadChanged proves a moved head is reported distinctly and nothing is
// credited to the expected head.
func TestE2EHeadChanged(t *testing.T) {
	observed := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	f := newE2EFixture(t,
		"FAKE_GH_HEAD="+observed,
		"FAKE_GH_REVIEWS=alice=APPROVED="+observed+",bob=APPROVED="+observed,
	)
	code, out := runWatchIn(t, f.bin, f.repoDir, f.env, f.args(e2eHead)...)
	if code != 3 {
		t.Fatalf("exit = %d, want 3; output:\n%s", code, out)
	}
	result := decodeLifecycle(t, out)
	if result.Status != review.StatusHeadChanged {
		t.Fatalf("status = %q, want head-changed", result.Status)
	}
	if result.ObservedHead != observed {
		t.Fatalf("observedHead = %q, want %q", result.ObservedHead, observed)
	}
	if len(result.Responded) != 0 {
		t.Fatalf("responded = %v, want none", result.Responded)
	}
}

// TestE2EClosedPR proves a closed/merged PR exits 4.
func TestE2EClosedPR(t *testing.T) {
	f := newE2EFixture(t, "FAKE_GH_HEAD="+e2eHead, "FAKE_GH_STATE=CLOSED")
	code, out := runWatchIn(t, f.bin, f.repoDir, f.env, f.args(e2eHead)...)
	if code != 4 {
		t.Fatalf("exit = %d, want 4; output:\n%s", code, out)
	}
	if status := decodeLifecycle(t, out).Status; status != review.StatusClosed {
		t.Fatalf("status = %q, want closed", status)
	}
}

// TestE2EAPIErrorsExitOne proves a total API failure is an error, never a
// timeout, and that no state baseline is faked.
func TestE2EAPIErrorsExitOne(t *testing.T) {
	f := newE2EFixture(t, "FAKE_GH_HEAD="+e2eHead, "FAKE_GH_FAIL=1")
	code, out := runWatchIn(t, f.bin, f.repoDir, f.env, f.args(e2eHead)...)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; output:\n%s", code, out)
	}
	result := decodeLifecycle(t, out)
	if result.Status != review.StatusError {
		t.Fatalf("status = %q, want error", result.Status)
	}
	if result.Error == "" {
		t.Fatal("an error result must carry a message")
	}
	if _, err := os.Stat(f.state); err == nil {
		t.Fatal("a round that never observed the PR must not leave a baseline state file")
	}
}

// TestE2EWaitForAny proves the explicit any-mode opt-in through the binary.
func TestE2EWaitForAny(t *testing.T) {
	f := newE2EFixture(t,
		"FAKE_GH_HEAD="+e2eHead,
		"FAKE_GH_REVIEWS=alice=APPROVED="+e2eHead,
	)
	code, out := runWatchIn(t, f.bin, f.repoDir, f.env, f.args(e2eHead, "--wait-for", "any")...)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; output:\n%s", code, out)
	}
	result := decodeLifecycle(t, out)
	if result.Status != review.StatusReady || len(result.Responded) != 1 {
		t.Fatalf("result = %+v, want ready with one responder", result)
	}
	if len(result.Outstanding) != 1 {
		t.Fatalf("outstanding = %v, want the non-responding author", result.Outstanding)
	}
}

// TestE2EStaleReviewDoesNotComplete proves a review on an older commit cannot
// complete the watch through the real binary.
func TestE2EStaleReviewDoesNotComplete(t *testing.T) {
	stale := "cccccccccccccccccccccccccccccccccccccccc"
	f := newE2EFixture(t,
		"FAKE_GH_HEAD="+e2eHead,
		"FAKE_GH_REVIEWS=alice=APPROVED="+stale+",bob=APPROVED="+stale,
	)
	code, out := runWatchIn(t, f.bin, f.repoDir, f.env, f.args(e2eHead)...)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (timeout); output:\n%s", code, out)
	}
	result := decodeLifecycle(t, out)
	if len(result.Outstanding) != 2 {
		t.Fatalf("outstanding = %v, want both authors uncredited", result.Outstanding)
	}
	for _, e := range result.Events {
		if e.Kind == review.EventKindReview && e.Binding != review.BindingNone {
			t.Fatalf("stale review must stay unbound, got binding=%q", e.Binding)
		}
	}
}

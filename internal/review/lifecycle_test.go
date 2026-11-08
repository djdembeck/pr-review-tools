package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testHead = "1111111111111111111111111111111111111111"
const otherHead = "2222222222222222222222222222222222222222"

// fakeBackend serves scripted snapshots and event pages. Each step consumes one
// entry; the last entry repeats so a long wait cannot index out of range.
type fakeBackend struct {
	snapshots []fakeSnapshot
	events    [][]Event
	snapIdx   int
	eventIdx  int
	// onSnapshot is called after each Snapshot read, letting a test mutate
	// files or counters at a precise point.
	onSnapshot func(n int)
	onEvents   func(context.Context) error
}

type fakeSnapshot struct {
	head   string
	closed bool
	err    error
}

func (f *fakeBackend) Snapshot(ctx context.Context) (PullSnapshot, error) {
	f.snapIdx++
	if f.onSnapshot != nil {
		f.onSnapshot(f.snapIdx)
	}
	if len(f.snapshots) == 0 {
		return PullSnapshot{Head: testHead, Open: true}, nil
	}
	idx := min(f.snapIdx-1, len(f.snapshots)-1)
	s := f.snapshots[idx]
	if s.err != nil {
		return PullSnapshot{}, s.err
	}
	head := s.head
	if head == "" {
		head = testHead
	}
	return PullSnapshot{
		Platform:   PlatformGitHub,
		Owner:      "owner",
		Repo:       "repo",
		PR:         1,
		Head:       head,
		Open:       !s.closed,
		ObservedAt: time.Now(),
	}, nil
}

func (f *fakeBackend) Events(ctx context.Context) ([]Event, error) {
	if f.onEvents != nil {
		if err := f.onEvents(ctx); err != nil {
			return nil, err
		}
	}
	if len(f.events) == 0 {
		return nil, nil
	}
	idx := min(f.eventIdx, len(f.events)-1)
	f.eventIdx++
	return f.events[idx], nil
}

// fakeClock advances deterministically so a bounded wait finishes instantly.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(d time.Duration) bool {
	c.now = c.now.Add(d)
	return true
}

// newTestConfig builds a lifecycle config writing into a temp state path.
func newTestConfig(t *testing.T, backend EventBackend, authors ...string) (LifecycleConfig, *fakeClock, string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "review-state.json")
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if len(authors) == 0 {
		authors = []string{"alice", "bob"}
	}
	cfg := LifecycleConfig{
		Platform:  PlatformGitHub,
		Owner:     "owner",
		Repo:      "repo",
		PR:        1,
		Head:      testHead,
		StatePath: statePath,
		Authors:   authors,
		WaitFor:   WaitForAll,
		Timeout:   10 * time.Minute,
		Poll:      time.Minute,
		Backend:   backend,
		Now:       clock.Now,
		Sleep:     clock.Sleep,
	}
	return cfg, clock, statePath
}

func submittedReview(id, author, commit string) Event {
	return Event{ID: id, Kind: EventKindReview, Author: author, State: "APPROVED", CommitID: commit}
}

func TestLifecycleRejectsLateObservation(t *testing.T) {
	for _, stage := range []string{"events", "post-snapshot"} {
		t.Run(stage, func(t *testing.T) {
			b := &fakeBackend{events: [][]Event{{submittedReview("r1", "alice", testHead)}}}
			cfg, clock, statePath := newTestConfig(t, b, "alice")
			if stage == "events" {
				b.onEvents = func(context.Context) error { clock.now = clock.now.Add(cfg.Timeout); return nil }
			} else {
				b.onSnapshot = func(n int) {
					if n == 2 {
						clock.now = clock.now.Add(cfg.Timeout)
					}
				}
			}
			result, err := RunLifecycle(cfg)
			if err != nil || result.Status != StatusTimeout || result.ObservedHead != testHead || result.Polls != 0 || len(result.Responded) != 0 {
				t.Fatalf("late observation accepted: result=%+v err=%v", result, err)
			}
			if _, err := os.Stat(statePath); !os.IsNotExist(err) {
				t.Fatalf("late baseline persisted: %v", err)
			}
		})
	}
}

func TestLifecycleRepeatedEventFailuresPreserveCursor(t *testing.T) {
	b := &fakeBackend{events: [][]Event{{submittedReview("r1", "bob", testHead)}}}
	cfg, _, statePath := newTestConfig(t, b, "alice", "bob")
	cfg.Timeout = cfg.Poll
	first, err := RunLifecycle(cfg)
	if err != nil || first.Status != StatusTimeout {
		t.Fatalf("seed result=%+v err=%v", first, err)
	}
	failure := errors.New("missing credentials")
	calls := 0
	b.onEvents = func(context.Context) error { calls++; return failure }
	cfg.Timeout = 30 * time.Minute
	result, err := RunLifecycle(cfg)
	if !errors.Is(err, failure) || result.Status != StatusError || calls != 3 || result.ObservedHead != testHead {
		t.Fatalf("failure threshold: result=%+v err=%v calls=%d", result, err, calls)
	}
	st, _, err := LoadState(statePath)
	if err != nil || st.Polls != first.Polls || len(st.Events) != len(first.Events) {
		t.Fatalf("cursor changed after failures: state=%+v err=%v", st, err)
	}
}

func TestLifecycleAPIFailureAfterBaselineRemainsError(t *testing.T) {
	b := &fakeBackend{events: [][]Event{{submittedReview("r1", "bob", testHead)}}}
	cfg, clock, statePath := newTestConfig(t, b, "alice", "bob")
	failure := errors.New("API authorization rejected")
	calls := 0
	b.onEvents = func(context.Context) error {
		calls++
		if calls == 1 {
			return nil
		}
		clock.now = clock.now.Add(cfg.Timeout)
		return failure
	}
	result, err := RunLifecycle(cfg)
	if !errors.Is(err, failure) || result.Status != StatusError || result.Polls != 1 ||
		len(result.Responded) != 1 || result.Responded[0] != "bob" ||
		len(result.Outstanding) != 1 || result.Outstanding[0] != "alice" {
		t.Fatalf("known error after baseline: result=%+v err=%v", result, err)
	}
	state, _, loadErr := LoadState(statePath)
	if loadErr != nil || state.Polls != 1 {
		t.Fatalf("baseline lost: state=%+v err=%v", state, loadErr)
	}
}

func TestLifecycleRecoveredFailureThenMissingReviewerTimesOut(t *testing.T) {
	b := &fakeBackend{events: [][]Event{{submittedReview("r1", "bob", testHead)}}}
	cfg, _, _ := newTestConfig(t, b, "alice", "bob")
	cfg.Timeout = 2 * cfg.Poll
	calls := 0
	b.onEvents = func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("temporary API failure")
		}
		return nil
	}
	result, err := RunLifecycle(cfg)
	if err != nil || result.Status != StatusTimeout || result.Polls != 1 ||
		len(result.Outstanding) != 1 || result.Outstanding[0] != "alice" {
		t.Fatalf("recovered failure was not cleared: result=%+v err=%v", result, err)
	}
}

func TestLifecycleTransientEventFailureRecovers(t *testing.T) {
	b := &fakeBackend{events: [][]Event{{submittedReview("r1", "alice", testHead)}}}
	calls := 0
	b.onEvents = func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("temporary API failure")
		}
		return nil
	}
	cfg, _, _ := newTestConfig(t, b, "alice")
	result, err := RunLifecycle(cfg)
	if err != nil || result.Status != StatusReady || calls != 2 || result.ObservedHead != testHead {
		t.Fatalf("transient failure: result=%+v err=%v calls=%d", result, err, calls)
	}
}

func TestLifecycleRoundDeadlineCancelsGitHubCLI(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
*headRefOid*) printf '%s' '{"data":{"repository":{"pullRequest":{"headRefOid":"` + testHead + `","state":"OPEN"}}}}' ;;
*) exec sleep 10 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg, _, _ := newTestConfig(t, githubEventBackend{owner: "o", repo: "r", pr: 7}, "alice")
	cfg.Now, cfg.Sleep, cfg.Timeout = time.Now, nil, 150*time.Millisecond
	start := time.Now()
	result, err := RunLifecycle(cfg)
	if err != nil || result.Status != StatusTimeout || result.ObservedHead != testHead {
		t.Fatalf("CLI deadline: result=%+v err=%v", result, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("round did not cancel gh process")
	}
}

// TestRequestTimeoutIsFetchErrorNotRoundTimeout proves the status decision is
// keyed on the round deadline, not on the observation error chain: a request
// that blows its own per-request budget (round budget intact) is a fetch
// error, while exhausting the round deadline is a timeout.
func TestRequestTimeoutIsFetchErrorNotRoundTimeout(t *testing.T) {
	cases := []struct {
		name    string
		failure error
		// advance exhausts the round clock in the same observation as the
		// fetch failure, so the clock state is a decoy: the sentinel in the
		// error chain, not the clock, decides the status.
		advance    bool
		wantStatus string
		wantErr    bool
		wantPolls  int
	}{
		{
			name:       "request-local deadline is a fetch error",
			failure:    fmt.Errorf("gh request timed out after 30s: %w", context.DeadlineExceeded),
			wantStatus: StatusError,
			wantErr:    true,
			wantPolls:  0,
		},
		{
			name:       "request-local deadline with exhausted round clock is still a fetch error",
			failure:    fmt.Errorf("gh request timed out after 30s: %w", context.DeadlineExceeded),
			advance:    true,
			wantStatus: StatusError,
			wantErr:    true,
			wantPolls:  0,
		},
		{
			name:       "request-local cancellation is a fetch error",
			failure:    fmt.Errorf("gh request canceled: %w", context.Canceled),
			wantStatus: StatusError,
			wantErr:    true,
			wantPolls:  0,
		},
		{
			name:       "round deadline exhaustion is a timeout",
			wantStatus: StatusTimeout,
			wantPolls:  1, // the successful baseline poll before the budget ran out
		},
		{
			name: "round-abort wrapped by the backend is a timeout",
			failure: fmt.Errorf("aborted: %w", fmt.Errorf("%w: %w",
				context.DeadlineExceeded, errRoundDeadline)),
			wantStatus: StatusTimeout,
			wantErr:    false,
			wantPolls:  0,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			b := &fakeBackend{}
			cfg, clock, _ := newTestConfig(t, b, "alice")
			cfg.Timeout = time.Minute
			if tt.failure != nil {
				b.onEvents = func(context.Context) error {
					if tt.advance {
						clock.now = clock.now.Add(cfg.Timeout)
					}
					return tt.failure
				}
			} else {
				// Let the fake clock run out the round budget without any
				// fetch failure: a pure deadline exhaustion.
				cfg.Poll = time.Minute
				_ = clock
			}
			result, err := RunLifecycle(cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if result.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q (result=%+v)", result.Status, tt.wantStatus, result)
			}
			if result.Polls != tt.wantPolls {
				t.Fatalf("polls = %d, want %d", result.Polls, tt.wantPolls)
			}
		})
	}
}

// TestLifecycleAllAuthorsSubmittedReviews covers the primary contract: every
// selected author must have a review bound to the current head.
func TestLifecycleAllAuthorsSubmittedReviews(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}, {head: testHead}},
		events: [][]Event{{
			submittedReview("r1", "alice", testHead),
			{ID: "c1", Kind: EventKindIssueComment, Author: "carol"},
		}, {
			submittedReview("r1", "alice", testHead),
			submittedReview("r2", "bob", testHead),
		}},
	}
	cfg, _, statePath := newTestConfig(t, backend)

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusReady {
		t.Fatalf("status = %q, want %q (outstanding=%v)", result.Status, StatusReady, result.Outstanding)
	}
	if len(result.Responded) != 2 {
		t.Fatalf("responded = %v, want alice and bob", result.Responded)
	}
	if len(result.Outstanding) != 0 {
		t.Fatalf("outstanding = %v, want none", result.Outstanding)
	}
	if result.Version != ResultVersion {
		t.Fatalf("result version = %d, want %d", result.Version, ResultVersion)
	}
	if result.Head != testHead {
		t.Fatalf("head = %q, want %q", result.Head, testHead)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file must exist after a round: %v", err)
	}
}

// TestLifecycleCleanEmptyReview verifies a review with no inline comments
// still completes its author: completion is the review object, not comments.
func TestLifecycleCleanEmptyReview(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}, {head: testHead}, {head: testHead}, {head: testHead}},
		events: [][]Event{{
			{ID: "r1", Kind: EventKindReview, Author: "alice", State: "COMMENTED", CommitID: testHead},
			{ID: "r2", Kind: EventKindReview, Author: "bob", State: "APPROVED", CommitID: testHead},
		}},
	}
	cfg, _, _ := newTestConfig(t, backend)

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusReady {
		t.Fatalf("status = %q, want ready; a comment-only review must count", result.Status)
	}
	if result.Polls != 1 {
		t.Fatalf("polls = %d, want 1: the baseline fetch already contains both reviews", result.Polls)
	}
}

// TestLifecycleIssueSummaryOnly counts a top-level comment first seen after
// the baseline as stable-head evidence, and labels it as such.
func TestLifecycleIssueSummaryOnly(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events: [][]Event{
			{{ID: "10", Kind: EventKindIssueComment, Author: "alice", Body: "old summary"}},
			{
				{ID: "10", Kind: EventKindIssueComment, Author: "alice", Body: "old summary"},
				{ID: "11", Kind: EventKindIssueComment, Author: "alice", Body: "new summary"},
				{ID: "12", Kind: EventKindIssueComment, Author: "bob", Body: "bob summary"},
			},
		},
	}
	cfg, _, _ := newTestConfig(t, backend)

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusReady {
		t.Fatalf("status = %q, want ready (outstanding=%v)", result.Status, result.Outstanding)
	}
	var sawStable, sawBaseline bool
	for _, e := range result.Events {
		if e.Kind != EventKindIssueComment {
			continue
		}
		if e.ID == "11" {
			if e.Binding != BindingStableHead {
				t.Fatalf("new issue comment binding = %q, want %q", e.Binding, BindingStableHead)
			}
			sawStable = true
		}
		if e.ID == "10" {
			if e.Binding != BindingNone || !e.Baseline {
				t.Fatalf("pre-existing comment must stay unbound and baseline-flagged, got binding=%q baseline=%v", e.Binding, e.Baseline)
			}
			sawBaseline = true
		}
	}
	if !sawStable || !sawBaseline {
		t.Fatalf("expected both the new and the pre-existing comment in events, got %+v", result.Events)
	}
}

// TestLifecyclePreExistingUnboundCommentDoesNotComplete proves an old top-level
// comment cannot establish current-head completion for a new wait.
func TestLifecyclePreExistingUnboundCommentDoesNotComplete(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events: [][]Event{
			{
				{ID: "10", Kind: EventKindIssueComment, Author: "alice", Body: "old"},
				{ID: "11", Kind: EventKindIssueComment, Author: "bob", Body: "old"},
			},
		},
	}
	cfg, _, _ := newTestConfig(t, backend)
	cfg.Timeout = 3 * time.Minute

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusTimeout {
		t.Fatalf("status = %q, want timeout: pre-existing unbound comments must not complete", result.Status)
	}
	if len(result.Outstanding) != 2 {
		t.Fatalf("outstanding = %v, want both authors", result.Outstanding)
	}
}

// TestLifecyclePreExistingBoundReviewCounts proves a review already on the head
// at baseline completes its author: the commit binding is the proof.
func TestLifecyclePreExistingBoundReviewCounts(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events: [][]Event{{
			submittedReview("r1", "alice", testHead),
			submittedReview("r2", "bob", testHead),
		}},
	}
	cfg, _, _ := newTestConfig(t, backend)

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusReady {
		t.Fatalf("status = %q, want ready: pre-existing head-bound reviews count", result.Status)
	}
}

// TestLifecyclePendingAndDismissedReviewsDoNotCount covers the two review
// states that must never complete an author.
func TestLifecyclePendingAndDismissedReviewsDoNotCount(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events: [][]Event{{
			{ID: "r1", Kind: EventKindReview, Author: "alice", State: "PENDING", CommitID: testHead},
			{ID: "r2", Kind: EventKindReview, Author: "bob", State: "DISMISSED", CommitID: testHead},
		}},
	}
	cfg, _, _ := newTestConfig(t, backend)
	cfg.Timeout = 3 * time.Minute

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusTimeout {
		t.Fatalf("status = %q, want timeout: pending/dismissed reviews must not count", result.Status)
	}
}

// TestLifecycleStaleReviewCommitDoesNotCount proves a review submitted against
// an earlier commit cannot complete the current head.
func TestLifecycleStaleReviewCommitDoesNotCount(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events: [][]Event{{
			submittedReview("r1", "alice", otherHead),
			submittedReview("r2", "bob", testHead),
		}},
	}
	cfg, _, _ := newTestConfig(t, backend)
	cfg.Timeout = 3 * time.Minute

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusTimeout {
		t.Fatalf("status = %q, want timeout: a stale-commit review must not complete", result.Status)
	}
	if len(result.Responded) != 1 || !strings.EqualFold(result.Responded[0], "bob") {
		t.Fatalf("responded = %v, want only bob", result.Responded)
	}
	for _, e := range result.Events {
		if e.ID == "r1" && e.Binding != BindingNone {
			t.Fatalf("stale review binding = %q, want none", e.Binding)
		}
	}
}

// TestLifecycleInlineCommentsDoNotComplete proves inline review comments are
// evidence only: a reviewer posting one comment in a multi-comment review has
// not finished.
func TestLifecycleInlineCommentsDoNotComplete(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events: [][]Event{{
			{ID: "c1", Kind: EventKindReviewComment, Author: "alice", CommitID: testHead, Path: "a.go", Line: 3},
			{ID: "c2", Kind: EventKindReviewComment, Author: "bob", CommitID: testHead},
		}},
	}
	cfg, _, _ := newTestConfig(t, backend)
	cfg.Timeout = 3 * time.Minute

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusTimeout {
		t.Fatalf("status = %q, want timeout: inline comments must not complete", result.Status)
	}
	if len(result.Events) != 2 {
		t.Fatalf("inline comments must still be collected as evidence, got %+v", result.Events)
	}
}

// TestLifecycleHeadChangeAborts proves a moved head returns the distinct
// head-changed status and never credits the collected events.
func TestLifecycleHeadChangeAborts(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: otherHead}},
		events:    [][]Event{{submittedReview("r1", "alice", otherHead)}},
	}
	cfg, _, _ := newTestConfig(t, backend)

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusHeadChanged {
		t.Fatalf("status = %q, want %q", result.Status, StatusHeadChanged)
	}
	if result.ObservedHead != otherHead {
		t.Fatalf("observedHead = %q, want %q", result.ObservedHead, otherHead)
	}
	if len(result.Responded) != 0 {
		t.Fatalf("responded = %v, want none: stale events must not be credited", result.Responded)
	}
}

// TestLifecycleHeadChangeDuringFetchDiscards proves events fetched while the
// head moved are discarded rather than credited.
func TestLifecycleHeadChangeDuringFetchDiscards(t *testing.T) {
	// Snapshot 1: matching head (proceed). Snapshot 2: after the fetch, the
	// head moved. The fetch's events must not count.
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}, {head: otherHead}},
		events:    [][]Event{{submittedReview("r1", "alice", testHead), submittedReview("r2", "bob", testHead)}},
	}
	cfg, _, statePath := newTestConfig(t, backend)

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusHeadChanged {
		t.Fatalf("status = %q, want %q", result.Status, StatusHeadChanged)
	}
	if len(result.Responded) != 0 {
		t.Fatalf("responded = %v, want none", result.Responded)
	}
	// No baseline ever completed, so nothing is persisted at all: a
	// half-initialized cursor must never be written, and the discarding
	// fetch's events must certainly not be on disk.
	if _, ok, err := LoadState(statePath); err != nil || ok {
		t.Fatalf("an uninitialized round must leave no state file: ok=%v err=%v", ok, err)
	}
}

// TestLifecycleClosedPR proves a closed/merged PR ends the round distinctly.
func TestLifecycleClosedPR(t *testing.T) {
	merged := &fakeBackend{snapshots: []fakeSnapshot{{head: testHead, closed: true}}}
	cfg, _, _ := newTestConfig(t, merged)
	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusClosed {
		t.Fatalf("status = %q, want %q", result.Status, StatusClosed)
	}
}

// TestLifecycleTimeoutPersistsAndResumeFillsGap is the central durability
// test: a round that times out keeps its cursor, and the resumed round credits
// an event that arrived entirely inside the gap between rounds.
func TestLifecycleTimeoutPersistsAndResumeFillsGap(t *testing.T) {
	// Round 1: nothing but the baseline; must time out with state on disk.
	round1 := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events:    [][]Event{{submittedReview("old", "carol", testHead)}},
	}
	cfg, clock, statePath := newTestConfig(t, round1)
	cfg.Timeout = 2 * time.Minute

	first, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("round 1 error: %v", err)
	}
	if first.Status != StatusTimeout {
		t.Fatalf("round 1 status = %q, want timeout", first.Status)
	}
	st, ok, err := LoadState(statePath)
	if err != nil || !ok {
		t.Fatalf("round 1 must persist state: ok=%v err=%v", ok, err)
	}
	if !st.BaselineComplete {
		t.Fatalf("round 1 must persist a complete baseline")
	}
	if st.Polls == 0 {
		t.Fatalf("round 1 must persist its poll count")
	}

	// The gap: alice and bob respond with head-bound reviews. A fresh
	// baseline would swallow them; resume must not.
	round2 := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events: [][]Event{{
			submittedReview("old", "carol", testHead),
			submittedReview("r1", "alice", testHead),
			submittedReview("r2", "bob", testHead),
		}},
	}
	resumedCfg := cfg
	resumedCfg.Backend = round2
	resumedCfg.Now = clock.Now
	resumedCfg.Sleep = clock.Sleep

	second, err := RunLifecycle(resumedCfg)
	if err != nil {
		t.Fatalf("round 2 error: %v", err)
	}
	if second.Status != StatusReady {
		t.Fatalf("round 2 status = %q, want ready (outstanding=%v)", second.Status, second.Outstanding)
	}
	if second.Polls <= first.Polls {
		t.Fatalf("resume must continue the poll count: round1=%d round2=%d", first.Polls, second.Polls)
	}
	// The events collected in round 1's baseline must not be lost.
	var sawOld bool
	for _, e := range second.Events {
		if e.ID == "old" {
			sawOld = true
		}
	}
	if !sawOld {
		t.Fatalf("resume must preserve previously collected events, got %+v", second.Events)
	}
}

// TestLifecycleDismissedReviewUncreditsAuthor is the consumer-visible
// transition: alice's review is credited while the wait is still running for
// bob, then the SAME review is dismissed. Readiness must recompute from the
// fresh snapshot rather than returning ready from cached responded[alice]: a
// dismissed review is not a response.
func TestLifecycleDismissedReviewUncreditsAuthor(t *testing.T) {
	dismissed := submittedReview("r1", "alice", testHead)
	dismissed.State = ReviewStateDismissed
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events: [][]Event{
			// Alice is credited from this page, but bob has not responded,
			// so the round keeps waiting.
			{submittedReview("r1", "alice", testHead)},
			// Same review ID, now dismissed.
			{dismissed},
			// Bob responds for real.
			{dismissed, submittedReview("r2", "bob", testHead)},
		},
	}
	cfg, _, _ := newTestConfig(t, backend)
	cfg.Timeout = 30 * time.Minute

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusTimeout {
		t.Fatalf("status = %q, want timeout: alice's dismissed review must never re-credit her (responded=%v)", result.Status, result.Responded)
	}
	if len(result.Responded) != 1 || !strings.EqualFold(result.Responded[0], "bob") {
		t.Fatalf("responded = %v, want only bob", result.Responded)
	}
	if len(result.Outstanding) != 1 || !strings.EqualFold(result.Outstanding[0], "alice") {
		t.Fatalf("outstanding = %v, want alice after her review was dismissed", result.Outstanding)
	}
	// The refreshed evidence must be the dismissed state, not the stale one.
	var r1 *CollectedEvent
	for i := range result.Events {
		if result.Events[i].ID == "r1" {
			r1 = &result.Events[i]
		}
	}
	if r1 == nil {
		t.Fatalf("review r1 must still be collected, got %+v", result.Events)
	}
	if r1.State != ReviewStateDismissed {
		t.Fatalf("r1 state = %q, want %q (same-ID updates must not be ignored)", r1.State, ReviewStateDismissed)
	}
	if r1.Binding != BindingNone {
		t.Fatalf("r1 binding = %q, want none", r1.Binding)
	}
}

// TestLifecycleResumePreservesStableHeadBinding proves a top-level comment's
// stable-head binding survives a resume: it is recomputed only when an event is
// refreshed, so a later fetch that omits it must not downgrade the saved
// evidence and silently un-credit the author.
func TestLifecycleResumePreservesStableHeadBinding(t *testing.T) {
	// Round 1 arms the baseline with no comments, then bob posts a summary.
	round1 := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events:    [][]Event{{}, {{ID: "c1", Kind: EventKindIssueComment, Author: "bob"}}},
	}
	cfg, clock, statePath := newTestConfig(t, round1, "alice", "bob")
	cfg.Timeout = 5 * time.Minute

	first, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("round 1 error: %v", err)
	}
	// Bob's summary and alice's review are still missing, so round 1 times
	// out with bob already credited.
	if first.Status != StatusTimeout {
		t.Fatalf("round 1 status = %q, want timeout", first.Status)
	}
	if len(first.Responded) != 1 || !strings.EqualFold(first.Responded[0], "bob") {
		t.Fatalf("round 1 responded = %v, want bob via his summary", first.Responded)
	}

	// Round 2's fresh fetch returns the same comment; resume must credit bob
	// again from the saved stable-head binding.
	round2 := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events:    [][]Event{{{ID: "c1", Kind: EventKindIssueComment, Author: "bob"}, submittedReview("r1", "alice", testHead)}},
	}
	resumed := cfg
	resumed.Backend = round2
	resumed.Now = clock.Now
	resumed.Sleep = clock.Sleep

	second, err := RunLifecycle(resumed)
	if err != nil {
		t.Fatalf("round 2 error: %v", err)
	}
	if second.Status != StatusReady {
		t.Fatalf("round 2 status = %q, want ready (responded=%v outstanding=%v)", second.Status, second.Responded, second.Outstanding)
	}
	var binding Binding
	for _, e := range second.Events {
		if e.ID == "c1" {
			binding = e.Binding
		}
	}
	if binding != BindingStableHead {
		t.Fatalf("c1 binding across resume = %q, want %q", binding, BindingStableHead)
	}
	if _, ok, err := LoadState(statePath); err != nil || !ok {
		t.Fatalf("state must stay durable: ok=%v err=%v", ok, err)
	}
}

// TestLifecycleBaselineArmedMarker proves the armed marker is emitted only
// after the durable baseline exists, so a caller can start CI on that signal.
func TestLifecycleBaselineArmedMarker(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events:    [][]Event{{{ID: "10", Kind: EventKindIssueComment, Author: "carol"}}},
	}
	cfg, _, statePath := newTestConfig(t, backend)
	cfg.Timeout = time.Minute

	var logs []string
	var stateAtMarker string
	cfg.Log = func(format string, args ...any) {
		line := strings.TrimSpace(format)
		logs = append(logs, line)
		if strings.Contains(line, "review-watch: armed") {
			// Snapshot the durable state at exactly the marker.
			if payload, err := os.ReadFile(statePath); err == nil {
				stateAtMarker = string(payload)
			}
		}
	}

	if _, err := RunLifecycle(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	armedAt := -1
	for i, line := range logs {
		if strings.Contains(line, "review-watch: armed") {
			armedAt = i
		}
	}
	if armedAt < 0 {
		t.Fatalf("expected a 'review-watch: armed' marker, logs=%v", logs)
	}
	if stateAtMarker == "" {
		t.Fatalf("state file must be durable at the armed marker")
	}
	var st State
	if err := json.Unmarshal([]byte(stateAtMarker), &st); err != nil {
		t.Fatalf("state at marker must be valid JSON: %v", err)
	}
	if !st.BaselineComplete {
		t.Fatalf("state at marker must record a complete baseline")
	}
}

// TestLifecycleMalformedStateIsFatal proves a malformed state file aborts
// rather than being silently reset.
func TestLifecycleMalformedStateIsFatal(t *testing.T) {
	backend := &fakeBackend{snapshots: []fakeSnapshot{{head: testHead}}}
	cfg, _, statePath := newTestConfig(t, backend)
	if err := os.WriteFile(statePath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	result, err := RunLifecycle(cfg)
	if err == nil {
		t.Fatal("expected a fatal error for malformed state")
	}
	if !IsStateError(err) {
		t.Fatalf("error must be a StateError, got %T: %v", err, err)
	}
	if result.Status != StatusError {
		t.Fatalf("status = %q, want %q", result.Status, StatusError)
	}
	// The malformed file must be preserved, not overwritten.
	payload, readErr := os.ReadFile(statePath)
	if readErr != nil || string(payload) != "{not json" {
		t.Fatalf("malformed state must not be rewritten, got %q (err=%v)", string(payload), readErr)
	}
}

// TestLifecycleMismatchedStateIsFatal covers each binding dimension: a state
// file from another round must never be reused.
func TestLifecycleMismatchedStateIsFatal(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*State)
	}{
		{"head", func(s *State) { s.Head = otherHead }},
		{"pr", func(s *State) { s.PR = 99 }},
		{"repo", func(s *State) { s.Repo = "other/repo" }},
		{"platform", func(s *State) { s.Platform = PlatformForgejo }},
		{"waitFor", func(s *State) { s.WaitFor = WaitForAny }},
		{"authors added", func(s *State) { s.Authors = []string{"alice", "bob", "carol"} }},
		{"authors removed", func(s *State) { s.Authors = []string{"alice"} }},
		{"baseline incomplete", func(s *State) { s.BaselineComplete = false }},
		{"version", func(s *State) {}}, // rewritten below: SaveState pins Version
		{"nil seen", func(s *State) { s.Seen = nil; s.BaselineComplete = true }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeBackend{snapshots: []fakeSnapshot{{head: testHead}}}
			cfg, clock, statePath := newTestConfig(t, backend)

			st := NewState(StateBinding{
				Platform: PlatformGitHub,
				Repo:     "owner/repo",
				PR:       1,
				Head:     testHead,
				WaitFor:  WaitForAll,
				Authors:  []string{"alice", "bob"},
			}, nil, clock.now)
			st.BaselineComplete = true
			tt.mutate(st)
			if err := SaveState(statePath, st, clock.now); err != nil {
				t.Fatalf("seed state: %v", err)
			}
			if tt.name == "version" {
				// SaveState always writes the current schema version, so a
				// future-version file must be forged directly.
				forgeStateVersion(t, statePath, StateVersion+1)
			}

			result, err := RunLifecycle(cfg)
			if tt.name == "nil seen" {
				// A nil Seen map is repaired on load because it is a
				// derivable map, not an identity binding.
				if err != nil {
					t.Fatalf("nil Seen must be initialized on load, got %v", err)
				}
				if result.Status != StatusTimeout {
					t.Fatalf("status = %q, want timeout", result.Status)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected a fatal state error for %s mismatch", tt.name)
			}
			if !IsStateError(err) {
				t.Fatalf("error must be a StateError, got %T: %v", err, err)
			}
		})
	}
}

// TestLifecycleAuthorSetIsCaseInsensitive proves re-casing or re-ordering the
// same author CSV is the same round, but adding a login is not.
func TestLifecycleAuthorSetIsCaseInsensitive(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events:    [][]Event{{submittedReview("r1", "ALICE", testHead)}},
	}
	cfg, clock, statePath := newTestConfig(t, backend, "Alice", "BOB")
	cfg.Timeout = time.Minute

	if _, err := RunLifecycle(cfg); err != nil {
		t.Fatalf("round 1 error: %v", err)
	}

	resumed := cfg
	resumed.Authors = []string{"bob", "alice"}
	resumed.Now = clock.Now
	resumed.Sleep = clock.Sleep
	if _, err := RunLifecycle(resumed); err != nil {
		t.Fatalf("re-cased/re-ordered authors must validate: %v", err)
	}

	changed := cfg
	changed.Authors = []string{"alice", "bob", "carol"}
	changed.Now = clock.Now
	changed.Sleep = clock.Sleep
	if _, err := RunLifecycle(changed); err == nil {
		t.Fatal("adding an author must invalidate the state")
	}
	_ = statePath
}

// TestLifecycleAPIErrorsRetryAndPreserveCursor proves a transient API failure
// neither loses the cursor nor counts as reviewer absence.
func TestLifecycleAPIErrorsRetryAndPreserveCursor(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}, {err: errors.New("503 backend unavailable")}, {head: testHead}},
		events:    [][]Event{{submittedReview("r1", "alice", testHead)}},
	}
	cfg, _, _ := newTestConfig(t, backend, "alice")
	cfg.Timeout = 5 * time.Minute

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("a transient snapshot error must be retried, got %v", err)
	}
	if result.Status != StatusReady {
		t.Fatalf("status = %q, want ready after retry (outstanding=%v)", result.Status, result.Outstanding)
	}
}

// TestLifecycleAllAPIErrorsIsErrorNotTimeout proves an unobservable round is an
// error, never a timeout: absence of evidence is not absence of reviews.
func TestLifecycleAllAPIErrorsIsErrorNotTimeout(t *testing.T) {
	backend := &fakeBackend{snapshots: []fakeSnapshot{{err: errors.New("401 bad credentials")}}}
	cfg, _, _ := newTestConfig(t, backend)
	cfg.Timeout = 2 * time.Minute

	result, err := RunLifecycle(cfg)
	if err == nil {
		t.Fatal("expected an error when no observation ever succeeded")
	}
	if result.Status != StatusError {
		t.Fatalf("status = %q, want %q", result.Status, StatusError)
	}
}

// TestLifecycleWaitForAny covers the explicit opt-in mode.
func TestLifecycleWaitForAny(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events:    [][]Event{{submittedReview("r1", "alice", testHead)}},
	}
	cfg, _, _ := newTestConfig(t, backend)
	cfg.WaitFor = WaitForAny
	cfg.Timeout = 2 * time.Minute

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusReady {
		t.Fatalf("status = %q, want ready with any-mode", result.Status)
	}
	if len(result.Responded) != 1 {
		t.Fatalf("responded = %v, want one author", result.Responded)
	}
	if len(result.Outstanding) != 1 {
		t.Fatalf("outstanding = %v, want the other author", result.Outstanding)
	}
}

// TestLifecycleDuplicateAuthorCSVIsDeduped proves repeating a login cannot
// inflate the all-mode denominator.
func TestLifecycleDuplicateAuthorCSVIsDeduped(t *testing.T) {
	backend := &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events:    [][]Event{{submittedReview("r1", "alice", testHead)}},
	}
	cfg, _, _ := newTestConfig(t, backend, "alice", "ALICE", "alice")
	cfg.Timeout = time.Minute

	result, err := RunLifecycle(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Authors) != 1 {
		t.Fatalf("authors = %v, want one deduped login", result.Authors)
	}
	if result.Status != StatusReady {
		t.Fatalf("status = %q, want ready", result.Status)
	}
}

// TestValidateLifecycleConfig covers the pre-network contract checks.
func TestValidateLifecycleConfig(t *testing.T) {
	base := LifecycleConfig{
		Platform:  PlatformGitHub,
		Owner:     "owner",
		Repo:      "repo",
		PR:        1,
		Head:      testHead,
		StatePath: "/tmp/state.json",
		Authors:   []string{"alice"},
		WaitFor:   WaitForAll,
		Backend:   &fakeBackend{},
	}
	if err := ValidateLifecycleConfig(base); err != nil {
		t.Fatalf("baseline config must validate: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*LifecycleConfig)
		want   string
	}{
		{"no pr", func(c *LifecycleConfig) { c.PR = 0 }, "positive PR number"},
		{"no repo", func(c *LifecycleConfig) { c.Repo = "" }, "owner/repo"},
		{"no head", func(c *LifecycleConfig) { c.Head = "" }, "--head"},
		{"no state", func(c *LifecycleConfig) { c.StatePath = "" }, "--state"},
		{"no authors", func(c *LifecycleConfig) { c.Authors = nil }, "--authors"},
		{"blank author", func(c *LifecycleConfig) { c.Authors = []string{" "} }, "empty login"},
		{"bad wait", func(c *LifecycleConfig) { c.WaitFor = "most" }, "--wait-for"},
		{"no backend", func(c *LifecycleConfig) { c.Backend = nil }, "backend"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.Authors = append([]string(nil), base.Authors...)
			tt.mutate(&cfg)
			err := ValidateLifecycleConfig(cfg)
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q must mention %q", err, tt.want)
			}
		})
	}
}

// forgeStateVersion rewrites a state file's schema version directly, which
// SaveState would never do.
func forgeStateVersion(t *testing.T, path string, version int) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	raw["version"] = version
	out, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

// TestStateFilePermissions proves the cursor is not group/other writable and
// that a writable state file is refused.
func TestStateFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := NewState(StateBinding{
		Platform: PlatformGitHub, Repo: "owner/repo", PR: 1, Head: testHead,
		WaitFor: WaitForAll, Authors: []string{"alice"},
	}, nil, now)
	st.BaselineComplete = true
	if err := SaveState(path, st, now); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state mode = %04o, want 0600", perm)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, _, err := LoadState(path); err == nil {
		t.Fatal("a group/other-writable state file must be refused")
	}
}

// TestStateRefusesSymlink proves a symlinked state path is rejected so the
// cursor cannot be redirected by another file.
func TestStateRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := NewState(StateBinding{
		Platform: PlatformGitHub, Repo: "owner/repo", PR: 1, Head: testHead,
		WaitFor: WaitForAll, Authors: []string{"alice"},
	}, nil, now)
	st.BaselineComplete = true
	if err := SaveState(target, st, now); err != nil {
		t.Fatalf("save: %v", err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, _, err := LoadState(link); err == nil {
		t.Fatal("a symlinked state path must be refused")
	}
}

// TestStateRefusesOtherUserOwnedFile proves a state file written by another
// user is refused even with a correct mode, so a cursor cannot be planted by
// a co-tenant of the host.
func TestStateRefusesOtherUserOwnedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := NewState(StateBinding{
		Platform: PlatformGitHub, Repo: "owner/repo", PR: 1, Head: testHead,
		WaitFor: WaitForAll, Authors: []string{"alice"},
	}, nil, now)
	st.BaselineComplete = true
	if err := SaveState(path, st, now); err != nil {
		t.Fatalf("save: %v", err)
	}
	if os.Getuid() == 0 {
		// Root can chown to anyone; never chown to itself.
		if err := os.Chown(path, 1, os.Getgid()); err != nil {
			t.Skipf("cannot chown to another uid as root: %v", err)
		}
	} else if err := os.Chown(path, 0, os.Getgid()); err != nil {
		t.Skipf("cannot chown to another uid on this host: %v", err)
	}
	_, _, err := LoadState(path)
	if err == nil {
		t.Fatal("a state file owned by another user must be refused")
	}
	var se *StateError
	if !errors.As(err, &se) {
		t.Fatalf("want StateError, got %T: %v", err, err)
	}
}

// TestStateOwnershipMismatchIsFatal forces a uid mismatch through the
// currentUID seam, so the foreign-owner refusal is exercised unconditionally
// on any host, independent of chown availability.
func TestStateOwnershipMismatchIsFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := NewState(StateBinding{
		Platform: PlatformGitHub, Repo: "owner/repo", PR: 1, Head: testHead,
		WaitFor: WaitForAll, Authors: []string{"alice"},
	}, nil, now)
	st.BaselineComplete = true
	if err := SaveState(path, st, now); err != nil {
		t.Fatalf("save: %v", err)
	}

	uid := currentUID()
	currentUID = func() int { return uid + 1 }
	t.Cleanup(func() { currentUID = os.Getuid })

	_, _, err := LoadState(path)
	if err == nil {
		t.Fatal("a state file whose owner does not match the current uid must be refused")
	}
	var se *StateError
	if !errors.As(err, &se) {
		t.Fatalf("want StateError, got %T: %v", err, err)
	}
}

// TestStateNoCredentialsOnDisk proves the cursor stores no token material.
func TestStateNoCredentialsOnDisk(t *testing.T) {
	cfg, _, statePath := newTestConfig(t, &fakeBackend{
		snapshots: []fakeSnapshot{{head: testHead}},
		events:    [][]Event{{submittedReview("r1", "alice", testHead)}},
	})
	cfg.Timeout = time.Minute
	if _, err := RunLifecycle(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	payload, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	lower := strings.ToLower(string(payload))
	for _, banned := range []string{"authorization", "token", "bearer", "password", "secret"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("state file must not contain %q: %s", banned, payload)
		}
	}
}

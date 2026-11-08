package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedGH records the argv of each gh call and returns canned stdout keyed
// by the GraphQL query or REST endpoint it detects.
type scriptedGH struct {
	mu    sync.Mutex
	calls [][]string
	// handler receives the full argv and returns stdout or an error.
	handler func(args []string) (string, error)
}

func (s *scriptedGH) run(_ context.Context, args []string, _ time.Duration) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string(nil), args...))
	s.mu.Unlock()
	return s.handler(args)
}

// argvContains reports whether any recorded call contains the token.
func (s *scriptedGH) argvContains(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, call := range s.calls {
		for _, a := range call {
			if strings.Contains(a, token) {
				return true
			}
		}
	}
	return false
}

func (s *scriptedGH) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// ghArg reports the value following flag in args, or "".
func ghArg(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestForgejoServerClampedPagesCompleteLifecycle(t *testing.T) {
	for _, kind := range []string{"review", "summary"} {
		t.Run(kind, func(t *testing.T) {
			snapshots := 0
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				page := r.URL.Query().Get("page")
				switch {
				case r.URL.Path == "/api/v1/repos/o/r/pulls/7":
					snapshots++
					fmt.Fprintf(w, `{"state":"open","head":{"sha":%q}}`, testHead)
				case strings.HasSuffix(r.URL.Path, "/reviews") && kind == "review", strings.Contains(r.URL.Path, "/issues/") && kind == "summary":
					if page == "1" {
						items := make([]map[string]any, 50)
						for i := range items {
							items[i] = map[string]any{"id": i + 1, "state": "APPROVED", "commit_id": otherHead, "user": map[string]string{"login": "other"}}
						}
						_ = json.NewEncoder(w).Encode(items)
					} else if page == "2" && (kind == "review" || snapshots >= 3) {
						fmt.Fprintf(w, `[{"id":51,"state":"APPROVED","commit_id":%q,"body":"done","user":{"login":"alice"}}]`, testHead)
					} else {
						fmt.Fprint(w, "[]")
					}
				default:
					fmt.Fprint(w, "[]")
				}
			}))
			defer srv.Close()
			setTestEnv(t, srv)
			cfg, _, _ := newTestConfig(t, forgejoEventBackend{owner: "o", repo: "r", pr: 7}, "alice")
			cfg.Platform, cfg.Owner, cfg.Repo, cfg.PR = PlatformForgejo, "o", "r", 7
			result, err := RunLifecycle(cfg)
			if err != nil || result.Status != StatusReady || len(result.Responded) != 1 {
				t.Fatalf("page-two response not credited: result=%+v err=%v", result, err)
			}
			found := false
			for _, event := range result.Events {
				if event.ID == "51" && event.Author == "alice" {
					found = event.Binding == BindingCommit || (kind == "summary" && event.Binding == BindingStableHead && !event.Baseline)
				}
			}
			if !found {
				t.Fatal("missing page-two completion evidence")
			}
		})
	}
}

func TestLifecycleDeadlineSpansSequentialRequests(t *testing.T) {
	calls := 0
	b := forgejoEventBackend{getFn: func(ctx context.Context, endpoint string, _ time.Duration) (string, error) {
		calls++
		if !strings.Contains(endpoint, "/reviews") {
			return `{"state":"open","head":{"sha":"` + testHead + `"}}`, nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			return `[{"id":1,"state":"APPROVED","commit_id":"` + testHead + `","user":{"login":"alice"}}]`, nil
		}
	}}
	cfg, _, _ := newTestConfig(t, b, "alice")
	cfg.Now, cfg.Sleep, cfg.Timeout = time.Now, nil, 80*time.Millisecond
	result, err := RunLifecycle(cfg)
	if err != nil || result.Status != StatusTimeout || result.ObservedHead != testHead || calls < 3 || calls >= 51 {
		t.Fatalf("round budget reset per request: result=%+v err=%v calls=%d", result, err, calls)
	}
}

func TestForgejoPaginationCapFailsClosed(t *testing.T) {
	b := forgejoEventBackend{getFn: func(context.Context, string, time.Duration) (string, error) {
		return `[{"id":1,"state":"APPROVED","commit_id":"head","user":{"login":"alice"}}]`, nil
	}}
	if events, _, err := b.reviews(context.Background()); err == nil || events != nil {
		t.Fatalf("capped reviews returned partial success: events=%v err=%v", events, err)
	}
	if events, err := b.issueComments(context.Background()); err == nil || events != nil {
		t.Fatalf("capped comments returned partial success: events=%v err=%v", events, err)
	}
}

func TestForgejoRoundDeadlineCancelsHTTP(t *testing.T) {
	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pulls/7") {
			fmt.Fprintf(w, `{"state":"open","head":{"sha":%q}}`, testHead)
			return
		}
		<-r.Context().Done()
		close(cancelled)
	}))
	defer srv.Close()
	setTestEnv(t, srv)
	cfg, _, _ := newTestConfig(t, forgejoEventBackend{owner: "o", repo: "r", pr: 7}, "alice")
	cfg.Now, cfg.Sleep = time.Now, nil
	cfg.Timeout = 100 * time.Millisecond
	start := time.Now()
	result, err := RunLifecycle(cfg)
	if err != nil || result.Status != StatusTimeout || result.ObservedHead != testHead {
		t.Fatalf("deadline result=%+v err=%v", result, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("round deadline did not bound HTTP")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("HTTP request was not cancelled")
	}
}

// TestGitHubBackendSnapshot proves the backend reads head and state from the
// authoritative GraphQL snapshot, not from the local checkout.
func TestGitHubBackendSnapshot(t *testing.T) {
	gh := &scriptedGH{handler: func(args []string) (string, error) {
		q := ghArg(args, "-F")
		if !strings.Contains(q, "headRefOid") {
			return "", fmt.Errorf("unexpected query: %s", q)
		}
		return `{"data":{"repository":{"pullRequest":{"headRefOid":"` + testHead + `","state":"OPEN"}}}}`, nil
	}}
	backend := githubEventBackend{owner: "o", repo: "r", pr: 3, runGHFn: gh.run}

	snap, err := backend.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Head != testHead || !snap.Open || snap.Merged {
		t.Fatalf("snapshot = %+v, want head %s open", snap, testHead)
	}
	if snap.Platform != PlatformGitHub || snap.PR != 3 {
		t.Fatalf("snapshot identity = %+v", snap)
	}
	for _, a := range gh.calls[0] {
		if strings.Contains(a, "shell") || strings.Contains(a, "&&") {
			t.Fatalf("gh must be invoked with an argument vector, got %v", gh.calls[0])
		}
	}
}

// TestGitHubBackendStateMapping covers merged/closed and error envelopes.
func TestGitHubBackendStateMapping(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		open    bool
		merged  bool
		wantErr string
	}{
		{"closed", `{"data":{"repository":{"pullRequest":{"headRefOid":"abc","state":"CLOSED"}}}}`, false, false, ""},
		{"merged", `{"data":{"repository":{"pullRequest":{"headRefOid":"abc","state":"MERGED"}}}}`, false, true, ""},
		{"graphql error", `{"errors":[{"message":"Could not resolve to a PullRequest"}]}`, false, false, "GraphQL error"},
		{"empty head", `{"data":{"repository":{"pullRequest":{"headRefOid":"","state":"OPEN"}}}}`, false, false, "head is empty"},
		{"no data", `{"data":{"repository":null}}`, false, false, "no pull request data"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			gh := &scriptedGH{handler: func([]string) (string, error) { return tt.payload, nil }}
			backend := githubEventBackend{owner: "o", repo: "r", pr: 1, runGHFn: gh.run}
			snap, err := backend.Snapshot(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if snap.Open != tt.open || snap.Merged != tt.merged {
				t.Fatalf("snapshot open=%v merged=%v, want open=%v merged=%v", snap.Open, snap.Merged, tt.open, tt.merged)
			}
		})
	}
}

// TestGitHubBackendEvents covers review states, root-only inline comments and
// top-level comments including pagination.
func TestGitHubBackendEvents(t *testing.T) {
	reviewsPage1 := `{"data":{"repository":{"pullRequest":{"reviews":{
      "nodes":[
        {"id":"R1","state":"APPROVED","submittedAt":"2026-01-01T00:00:00Z","body":"","url":"u1","commit":{"oid":"` + testHead + `"},"author":{"login":"alice"}},
        {"id":"R2","state":"PENDING","submittedAt":null,"body":"","url":"u2","commit":{"oid":"` + testHead + `"},"author":{"login":"bob"}}
      ],
      "pageInfo":{"hasNextPage":false,"endCursor":"c1"}}}}},"errors":[]}`
	threads := `{"data":{"repository":{"pullRequest":{"reviewThreads":{
      "nodes":[
        {"id":"T1","comments":{"nodes":[
           {"databaseId":9001,"body":"inline root","author":{"login":"alice"},"path":"a.go","line":12,"originalLine":10,"createdAt":"2026-01-01T00:00:00Z","url":"u9"},
           {"databaseId":9002,"body":"a reply","author":{"login":"bob"},"path":"a.go","line":13,"createdAt":"2026-01-01T00:00:01Z"}
        ]}},
        {"id":"T2","comments":{"nodes":[]}}
      ],
      "pageInfo":{"hasNextPage":false,"endCursor":"c2"}}}}},"errors":[]}`
	issues := `[[{"id":500,"body":"summary","created_at":"2026-01-01T00:00:02Z","html_url":"u5","user":{"login":"carol"}}]]`

	gh := &scriptedGH{handler: func(args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "reviews(first"):
			return reviewsPage1, nil
		case strings.Contains(joined, "reviewThreads(first"):
			return threads, nil
		case strings.Contains(joined, "issues/1/comments"):
			if !containsArg(args, "--paginate") {
				return "", errors.New("issue comments must be paginated")
			}
			return issues, nil
		default:
			return "", fmt.Errorf("unexpected gh invocation: %v", args)
		}
	}}
	backend := githubEventBackend{owner: "o", repo: "r", pr: 1, runGHFn: gh.run}

	events, err := backend.Events(context.Background())
	if err != nil {
		t.Fatalf("events: %v", err)
	}

	byKey := map[string]Event{}
	for _, e := range events {
		byKey[e.Key()] = e
	}
	if got := byKey["review:R1"]; got.State != "APPROVED" || got.CommitID != testHead || got.Author != "alice" {
		t.Fatalf("R1 = %+v", got)
	}
	if got := byKey["review:R2"]; got.State != "PENDING" {
		t.Fatalf("R2 = %+v, want PENDING preserved", got)
	}
	if got, ok := byKey["review_comment:9001"]; !ok || got.Path != "a.go" || got.Line != 12 {
		t.Fatalf("root inline comment = %+v ok=%v", got, ok)
	}
	if _, ok := byKey["review_comment:9002"]; ok {
		t.Fatal("a reply must not be collected as a root review comment")
	}
	if got := byKey["issue_comment:500"]; got.Author != "carol" || got.Body != "summary" {
		t.Fatalf("issue comment = %+v", got)
	}
	// Deterministic ordering: reviews before review comments before issue
	// comments.
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4: %+v", len(events), events)
	}
	wantOrder := []EventKind{EventKindReview, EventKindReview, EventKindReviewComment, EventKindIssueComment}
	for i, want := range wantOrder {
		if events[i].Kind != want {
			t.Fatalf("event %d kind = %q, want %q (events=%+v)", i, events[i].Kind, want, events)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestGitHubBackendErrorsAreNotAbsence proves an API failure surfaces as an
// error, never as an empty event list.
func TestGitHubBackendErrorsAreNotAbsence(t *testing.T) {
	gh := &scriptedGH{handler: func([]string) (string, error) {
		return "", errors.New("gh command failed: HTTP 502")
	}}
	backend := githubEventBackend{owner: "o", repo: "r", pr: 1, runGHFn: gh.run}
	if _, err := backend.Events(context.Background()); err == nil {
		t.Fatal("a failed fetch must return an error, not an empty list")
	}
}

// TestGitHubBackendPagination covers multi-page review and thread reads.
func TestGitHubBackendPagination(t *testing.T) {
	var reviewPages, threadPages int
	gh := &scriptedGH{handler: func(args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "reviews(first"):
			reviewPages++
			if reviewPages == 1 {
				return `{"data":{"repository":{"pullRequest":{"reviews":{
                  "nodes":[{"id":"R1","state":"COMMENTED","commit":{"oid":"` + testHead + `"},"author":{"login":"alice"}}],
                  "pageInfo":{"hasNextPage":true,"endCursor":"p2"}}}}},"errors":[]}`, nil
			}
			if !containsArg(args, "after=p2") {
				return "", errors.New("second review page must request the cursor")
			}
			return `{"data":{"repository":{"pullRequest":{"reviews":{
                  "nodes":[{"id":"R2","state":"APPROVED","commit":{"oid":"` + testHead + `"},"author":{"login":"bob"}}],
                  "pageInfo":{"hasNextPage":false,"endCursor":"p3"}}}}},"errors":[]}`, nil
		case strings.Contains(joined, "reviewThreads(first"):
			threadPages++
			if threadPages == 1 {
				return `{"data":{"repository":{"pullRequest":{"reviewThreads":{
                  "nodes":[{"id":"T1","comments":{"nodes":[{"databaseId":1,"body":"a","author":{"login":"alice"}}]}}],
                  "pageInfo":{"hasNextPage":true,"endCursor":"t2"}}}}},"errors":[]}`, nil
			}
			return `{"data":{"repository":{"pullRequest":{"reviewThreads":{
                  "nodes":[{"id":"T2","comments":{"nodes":[{"databaseId":2,"body":"b","author":{"login":"bob"}}]}}],
                  "pageInfo":{"hasNextPage":false,"endCursor":"t3"}}}}},"errors":[]}`, nil
		case strings.Contains(joined, "issues/1/comments"):
			return `[]`, nil
		default:
			return "", fmt.Errorf("unexpected gh invocation: %v", args)
		}
	}}
	backend := githubEventBackend{owner: "o", repo: "r", pr: 1, runGHFn: gh.run}
	events, err := backend.Events(context.Background())
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if reviewPages != 2 || threadPages != 2 {
		t.Fatalf("pagination incomplete: reviewPages=%d threadPages=%d", reviewPages, threadPages)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4 across both pages: %+v", len(events), events)
	}
}

// TestForgejoBackendSnapshot proves head/state come from the pull request
// object and that a missing head is an error.
func TestForgejoBackendSnapshot(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/repos/o/r/pulls/7":
			_, _ = w.Write([]byte(`{"number":7,"state":"open","merged":false,"head":{"sha":"` + testHead + `"}}`))
		case "/api/v1/repos/o/r/pulls/8":
			_, _ = w.Write([]byte(`{"number":8,"state":"closed","merged":true,"head":{"sha":"` + testHead + `"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	setTestEnv(t, srv)

	backend := forgejoEventBackend{owner: "o", repo: "r", pr: 7}
	snap, err := backend.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Head != testHead || !snap.Open {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Platform != PlatformForgejo {
		t.Fatalf("platform = %q", snap.Platform)
	}

	merged := forgejoEventBackend{owner: "o", repo: "r", pr: 8}
	snap, err = merged.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("merged snapshot: %v", err)
	}
	if snap.Open || !snap.Merged {
		t.Fatalf("merged snapshot = %+v, want closed+merged", snap)
	}
}

// TestForgejoBackendEvents covers dismissed reviews, pending reviews,
// commit_id, inline comments and issue comments.
func TestForgejoBackendEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if page := r.URL.Query().Get("page"); page != "" && page != "1" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		switch {
		case r.URL.Path == "/api/v1/repos/o/r/pulls/7/reviews":
			_, _ = w.Write([]byte(`[
              {"id":1,"state":"APPROVED","dismissed":false,"commit_id":"` + testHead + `","submitted_at":"2026-01-01T00:00:00Z","user":{"login":"alice"}},
              {"id":2,"state":"REQUEST_CHANGES","dismissed":true,"commit_id":"` + testHead + `","submitted_at":"2026-01-01T00:00:00Z","user":{"login":"bob"}},
              {"id":3,"state":"PENDING","dismissed":false,"commit_id":"","submitted_at":null,"user":{"login":"carol"}},
              {"id":4,"state":"COMMENT","dismissed":false,"commit_id":"` + otherHead + `","submitted_at":"2026-01-01T00:00:00Z","user":{"login":"dave"}}
            ]`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/repos/o/r/pulls/7/reviews/") && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = w.Write([]byte(`[{"id":91,"body":"inline","path":"b.go","position":5,"created_at":"2026-01-01T00:00:01Z","commit_id":"` + testHead + `","user":{"login":"alice"}}]`))
		case r.URL.Path == "/api/v1/repos/o/r/issues/7/comments":
			_, _ = w.Write([]byte(`[{"id":900,"body":"top level","created_at":"2026-01-01T00:00:02Z","html_url":"h","user":{"login":"carol"}}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	setTestEnv(t, srv)

	backend := forgejoEventBackend{owner: "o", repo: "r", pr: 7}
	events, err := backend.Events(context.Background())
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	byKey := map[string]Event{}
	for _, e := range events {
		byKey[e.Key()] = e
	}

	if got := byKey["review:1"]; got.State != "APPROVED" || got.CommitID != testHead {
		t.Fatalf("approved review = %+v", got)
	}
	if got := byKey["review:2"]; got.State != ReviewStateDismissed {
		t.Fatalf("dismissed review state = %q, want DISMISSED", got.State)
	}
	if got := byKey["review:3"]; got.State != ReviewStatePending {
		t.Fatalf("pending review state = %q", got.State)
	}
	if got := byKey["review:4"]; got.CommitID != otherHead {
		t.Fatalf("stale review commit = %q, want %q", got.CommitID, otherHead)
	}
	if got, ok := byKey["review_comment:91"]; !ok || got.Path != "b.go" || got.Line != 5 {
		t.Fatalf("inline comment = %+v ok=%v", got, ok)
	}
	if got, ok := byKey["issue_comment:900"]; !ok || got.Author != "carol" {
		t.Fatalf("issue comment = %+v ok=%v", got, ok)
	}
}

// TestForgejoBackendErrorStatus proves a non-2xx response is an error, not an
// empty result, and carries the platform message.
func TestForgejoBackendErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"token is required"}`))
	}))
	defer srv.Close()
	setTestEnv(t, srv)

	backend := forgejoEventBackend{owner: "o", repo: "r", pr: 7}
	_, err := backend.Events(context.Background())
	if err == nil {
		t.Fatal("a 401 must be an error")
	}
	if !strings.Contains(err.Error(), "token is required") {
		t.Fatalf("error = %v, want the platform message", err)
	}
}

// TestForgejoBackendTimeoutIsExplicit proves a slow request reports a timeout
// instead of hanging or returning empty data.
func TestForgejoBackendTimeoutIsExplicit(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()
	setTestEnv(t, srv)

	backend := forgejoEventBackend{owner: "o", repo: "r", pr: 7, timeout: 50 * time.Millisecond}
	_, err := backend.Events(context.Background())
	if err == nil {
		t.Fatal("a slow request must fail")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "timed out") {
		t.Fatalf("error = %v, want an explicit timeout", err)
	}
}

// TestNewEventBackend covers platform dispatch and the unknown-platform error.
func TestNewEventBackend(t *testing.T) {
	for _, p := range []Platform{PlatformGitHub, PlatformForgejo} {
		backend, err := NewEventBackend(p, "o", "r", 1)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if backend == nil {
			t.Fatalf("%s: nil backend", p)
		}
	}
	if _, err := NewEventBackend("gitlab", "o", "r", 1); err == nil {
		t.Fatal("an unsupported platform must error")
	}
}

// TestNormalizeAuthors covers trimming, empty removal and case-insensitive
// dedupe.
func TestNormalizeAuthors(t *testing.T) {
	got := NormalizeAuthors([]string{" alice ", "ALICE", "", "bob", "Bob", " "})
	want := []string{"alice", "bob"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if !strings.EqualFold(got[i], want[i]) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestReviewStateClassification pins the pending/dismissed/submitted split.
func TestReviewStateClassification(t *testing.T) {
	cases := map[string]bool{
		"PENDING":           false,
		"DISMISSED":         false,
		"":                  false,
		"APPROVED":          true,
		"CHANGES_REQUESTED": true,
		"REQUEST_CHANGES":   true,
		"COMMENTED":         true,
		"COMMENT":           true,
		"stale":             true,
	}
	for state, want := range cases {
		if got := ReviewStateCountsAsSubmission(state); got != want {
			t.Fatalf("ReviewStateCountsAsSubmission(%q) = %v, want %v", state, got, want)
		}
	}
}

// TestEventKeySeparatesKinds proves Forgejo's shared numeric ID space cannot
// collide across kinds.
func TestEventKeySeparatesKinds(t *testing.T) {
	a := Event{ID: "7", Kind: EventKindReview}
	b := Event{ID: "7", Kind: EventKindIssueComment}
	if a.Key() == b.Key() {
		t.Fatalf("keys must differ by kind: %q", a.Key())
	}
}

// TestSortEventsNumeric proves numeric IDs sort numerically, not as strings.
func TestSortEventsNumeric(t *testing.T) {
	events := []Event{
		{ID: "10", Kind: EventKindReview},
		{ID: "9", Kind: EventKindReview},
		{ID: "100", Kind: EventKindReview},
	}
	sortEvents(events)
	want := []string{"9", "10", "100"}
	for i := range want {
		if events[i].ID != want[i] {
			t.Fatalf("sorted = %v, want %v", events, want)
		}
	}
}

// TestStateJSONRoundTrip proves the durable schema survives an encode/decode
// cycle with every field intact.
func TestStateJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := NewState(StateBinding{
		Platform: PlatformForgejo, Repo: "o/r", PR: 4, Head: testHead,
		WaitFor: WaitForAny, Authors: []string{"alice", "bob"},
	}, []string{"alice"}, now)
	st.BaselineComplete = true
	st.Events = []Event{submittedReview("r1", "alice", testHead)}
	st.Seen = map[string]bool{"review:r1": true}
	st.Polls = 3

	payload, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back State
	if err := json.Unmarshal(payload, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Version != StateVersion || back.Platform != PlatformForgejo || back.PR != 4 {
		t.Fatalf("round trip identity = %+v", back)
	}
	if len(back.Events) != 1 || back.Events[0].ID != "r1" {
		t.Fatalf("round trip events = %+v", back.Events)
	}
	if !back.Seen["review:r1"] || back.Polls != 3 {
		t.Fatalf("round trip cursor = %+v", back)
	}
}

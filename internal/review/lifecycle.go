package review

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Lifecycle statuses. They are the machine-readable result of a watch round;
// the CLI maps them to the documented exit codes.
const (
	StatusReady       = "ready"
	StatusTimeout     = "timeout"
	StatusHeadChanged = "head-changed"
	StatusClosed      = "closed"
	StatusError       = "error"
)

// Wait modes.
const (
	WaitForAll = "all"
	WaitForAny = "any"
)

// ResultVersion is the version of the lifecycle result object printed by the
// watch CLI in event mode.
const ResultVersion = 1

// Default lifecycle bounds. They mirror the documented CLI defaults.
const (
	DefaultLifecycleTimeout = 30 * time.Minute
	DefaultLifecyclePoll    = 30 * time.Second
)

// errRoundDeadline wraps an observation error caused by the round deadline
// itself, as distinct from a request-local timeout inside one fetch. The
// lifecycle maps only this sentinel to StatusTimeout; every other fetch
// failure is a fatal error.
var errRoundDeadline = errors.New("round deadline exceeded")

// CollectedEvent is one event in the lifecycle result. It carries the raw
// evidence (including body text) but is never executed as instructions.
type CollectedEvent struct {
	ID        string    `json:"id"`
	Kind      EventKind `json:"kind"`
	Author    string    `json:"author"`
	State     string    `json:"state,omitempty"`
	CommitID  string    `json:"commitId,omitempty"`
	CreatedAt string    `json:"createdAt,omitempty"`
	Path      string    `json:"path,omitempty"`
	Line      int       `json:"line,omitempty"`
	URL       string    `json:"url,omitempty"`
	Binding   Binding   `json:"binding"`
	Baseline  bool      `json:"baseline"`
	Body      string    `json:"body,omitempty"`
}

// LifecycleResult is the versioned machine-readable outcome of a lifecycle
// watch round.
type LifecycleResult struct {
	Version      int              `json:"version"`
	Status       string           `json:"status"`
	Platform     Platform         `json:"platform"`
	Repo         string           `json:"repo"`
	PR           int              `json:"pr"`
	Head         string           `json:"head"`
	ObservedHead string           `json:"observedHead,omitempty"`
	StatePath    string           `json:"statePath"`
	WaitFor      string           `json:"waitFor"`
	Authors      []string         `json:"authors"`
	Responded    []string         `json:"responded"`
	Outstanding  []string         `json:"outstanding"`
	Events       []CollectedEvent `json:"events"`
	Baseline     int              `json:"baselineEvents"`
	Polls        int              `json:"polls"`
	ElapsedMS    int64            `json:"elapsedMs"`
	Error        string           `json:"error,omitempty"`
}

// LifecycleConfig is everything a lifecycle round needs. Backend, Log and Now
// are injectable so the machine is testable without processes or wall time.
type LifecycleConfig struct {
	Platform       Platform
	Owner          string
	Repo           string
	PR             int
	Head           string
	StatePath      string
	Authors        []string
	TrustedAuthors []string
	WaitFor        string
	Timeout        time.Duration
	Poll           time.Duration
	Backend        EventBackend
	Log            func(format string, args ...any)
	Now            func() time.Time
	// Sleep is the poll wait; tests replace it to advance a fake clock
	// instantly. It must return false when interrupted, aborting the wait.
	Sleep func(d time.Duration) bool
}

// ValidateLifecycleConfig enforces the lifecycle contract before any network
// access: an exact author set, a known wait mode, a state path and a head.
func ValidateLifecycleConfig(cfg LifecycleConfig) error {
	if cfg.PR <= 0 {
		return fmt.Errorf("lifecycle mode requires a positive PR number")
	}
	if cfg.Repo == "" || cfg.Owner == "" {
		return fmt.Errorf("lifecycle mode requires an owner/repo")
	}
	if cfg.Head == "" {
		return fmt.Errorf("lifecycle mode requires --head (the PR head SHA reviews must bind to)")
	}
	if cfg.StatePath == "" {
		return fmt.Errorf("lifecycle mode requires --state PATH")
	}
	if len(cfg.Authors) == 0 {
		return fmt.Errorf("lifecycle mode requires --authors with the exact selected logins")
	}
	for _, a := range cfg.Authors {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("lifecycle mode: --authors contains an empty login")
		}
	}
	switch cfg.WaitFor {
	case WaitForAll, WaitForAny:
	default:
		return fmt.Errorf("lifecycle mode: --wait-for must be %q or %q, got %q", WaitForAll, WaitForAny, cfg.WaitFor)
	}
	if cfg.Backend == nil {
		return fmt.Errorf("lifecycle mode: no event backend")
	}
	return nil
}

// RunLifecycle executes one bounded lifecycle round: initialize or resume the
// durable state, then poll until all (or any) selected authors respond, the
// deadline passes, the head changes, the PR closes, or a fatal error occurs.
// A returned error is fatal (exit 1); every other outcome is a Result.
func RunLifecycle(cfg LifecycleConfig) (LifecycleResult, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultLifecycleTimeout
	}
	if cfg.Poll <= 0 {
		cfg.Poll = DefaultLifecyclePoll
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepReal
	}
	cfg.Authors = NormalizeAuthors(cfg.Authors)

	repoFull := cfg.Owner + "/" + cfg.Repo
	result := LifecycleResult{
		Version:   ResultVersion,
		Platform:  cfg.Platform,
		Repo:      repoFull,
		PR:        cfg.PR,
		Head:      cfg.Head,
		StatePath: cfg.StatePath,
		WaitFor:   cfg.WaitFor,
		Authors:   append([]string(nil), cfg.Authors...),
		Events:    []CollectedEvent{},
		Responded: []string{},
	}

	start := cfg.Now()
	if err := ValidateLifecycleConfig(cfg); err != nil {
		return finishError(result, cfg, start, err)
	}
	bind := StateBinding{
		Platform: cfg.Platform,
		Repo:     repoFull,
		PR:       cfg.PR,
		Head:     cfg.Head,
		WaitFor:  cfg.WaitFor,
		Authors:  cfg.Authors,
	}

	st, resumed, err := LoadState(cfg.StatePath)
	if err != nil {
		return finishError(result, cfg, start, err)
	}
	if resumed {
		if err := st.ValidateBinding(bind); err != nil {
			return finishError(result, cfg, start, err)
		}
		cfg.Log("- Resuming state for %s #%d at head %s (%d event(s), %d poll(s))",
			repoFull, cfg.PR, shortSHA(cfg.Head), len(st.Events), st.Polls)
	} else {
		st = NewState(bind, cfg.TrustedAuthors, start)
		cfg.Log("- Initializing state %s for %s #%d at head %s; waiting for %s",
			cfg.StatePath, repoFull, cfg.PR, shortSHA(cfg.Head), authorLabel(cfg))
	}

	deadline := start.Add(cfg.Timeout)
	consecutiveFailures := 0
	var observationErr error
	armed := false
	// Use a real duration, not an injected clock timestamp, for cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), max(time.Duration(0), deadline.Sub(cfg.Now())))
	defer cancel()

	for ctx.Err() == nil && cfg.Now().Before(deadline) {
		snapshot, status, err := observe(ctx, cfg)
		if err != nil {
			observationErr = err
			consecutiveFailures++
			cfg.Log("- Observation failed: %v (keeping cursor; failure %d/3)", err, consecutiveFailures)
			if consecutiveFailures >= 3 || ctx.Err() != nil || !maybeSleep(cfg, cfg.Poll, deadline) {
				break
			}
			continue
		}
		result.ObservedHead = snapshot.Head
		if ctx.Err() != nil || !cfg.Now().Before(deadline) {
			break
		}
		if status != "" {
			saveQuietly(cfg, st)
			result.Status = status
			fillEvaluation(&result, st)
			result.ElapsedMS = cfg.Now().Sub(start).Milliseconds()
			if status == StatusClosed {
				cfg.Log("- PR #%d is no longer open (%s); stopping", cfg.PR, snapshotState(snapshot))
			} else {
				cfg.Log("- Head changed: expected %s, observed %s; start a new state path for the new head",
					shortSHA(cfg.Head), shortSHA(snapshot.Head))
			}
			return result, nil
		}

		events, err := cfg.Backend.Events(ctx)
		if err != nil {
			observationErr = err
			consecutiveFailures++
			cfg.Log("- Event fetch failed: %v (keeping cursor; failure %d/3)", err, consecutiveFailures)
			if consecutiveFailures >= 3 || ctx.Err() != nil || !maybeSleep(cfg, cfg.Poll, deadline) {
				break
			}
			continue
		}
		// Re-read head after the fetch: events collected across a head move
		// must never be credited to the old head.
		after, statusAfter, err := observe(ctx, cfg)
		if err != nil {
			observationErr = err
			consecutiveFailures++
			cfg.Log("- Post-fetch observation failed: %v (keeping cursor; failure %d/3)", err, consecutiveFailures)
			if consecutiveFailures >= 3 || ctx.Err() != nil || !maybeSleep(cfg, cfg.Poll, deadline) {
				break
			}
			continue
		}
		result.ObservedHead = after.Head
		if ctx.Err() != nil || !cfg.Now().Before(deadline) {
			break
		}
		if statusAfter != "" {
			saveQuietly(cfg, st)
			result.Status = statusAfter
			fillEvaluation(&result, st)
			result.ElapsedMS = cfg.Now().Sub(start).Milliseconds()
			cfg.Log("- Head/state changed during fetch (observed head %s); discarding this fetch",
				shortSHA(after.Head))
			return result, nil
		}

		consecutiveFailures = 0
		observationErr = nil
		changed := mergeEvents(st, events, cfg.Head, !st.BaselineComplete)
		if !st.BaselineComplete {
			st.BaselineComplete = true
			st.BaselineAt = cfg.Now().UTC().Format(time.RFC3339Nano)
			cfg.Log("- Baseline: %d pre-existing event(s) recorded; %d review(s) already on head %s",
				len(st.Events), countBoundReviews(st), shortSHA(cfg.Head))
		}
		st.Polls++
		if err := SaveState(cfg.StatePath, st, cfg.Now()); err != nil {
			return finishError(result, cfg, start, err)
		}
		if !armed {
			// The cursor and baseline are durable on disk from here on, so a
			// caller may start CI or any other work that moves the head. This
			// marker is the only readiness signal callers should wait for.
			armed = true
			cfg.Log("review-watch: armed")
		}
		if changed > 0 || st.Polls == 1 {
			cfg.Log("- Poll %d: %d new event(s) (%s elapsed)", st.Polls, changed, cfg.Now().Sub(start).Round(time.Second))
		}

		fillEvaluation(&result, st)
		if ctx.Err() != nil || !cfg.Now().Before(deadline) {
			break
		}
		if readySelected(result.WaitFor, result.Responded, result.Authors) {
			result.Status = StatusReady
			result.ElapsedMS = cfg.Now().Sub(start).Milliseconds()
			cfg.Log("- Readiness reached: responded=%v outstanding=%v", result.Responded, result.Outstanding)
			return result, nil
		}

		if !maybeSleep(cfg, cfg.Poll, deadline) {
			break
		}
	}

	saveQuietly(cfg, st)
	fillEvaluation(&result, st)
	result.ElapsedMS = cfg.Now().Sub(start).Milliseconds()
	if consecutiveFailures >= 3 {
		return finishError(result, cfg, start, fmt.Errorf("three consecutive PR observation failures: %w", observationErr))
	}
	// Only an observation aborted by the round deadline itself is a timeout:
	// the backends wrap such errors with errRoundDeadline. A request-local
	// timeout (the per-request 30s budget) is an ordinary fetch error even if
	// the round budget was also exhausted in the meantime.
	if observationErr != nil && !errors.Is(observationErr, errRoundDeadline) {
		return finishError(result, cfg, start, observationErr)
	}
	result.Status = StatusTimeout
	cfg.Log("- Timed out after %s; outstanding=%v (progress preserved in %s)",
		cfg.Timeout, result.Outstanding, cfg.StatePath)
	return result, nil
}

// observe reads the authoritative snapshot and classifies it against the
// configured head. It returns an empty status when the round may proceed.
func observe(ctx context.Context, cfg LifecycleConfig) (PullSnapshot, string, error) {
	snapshot, err := cfg.Backend.Snapshot(ctx)
	if err != nil {
		return PullSnapshot{}, "", err
	}
	if snapshot.Head == "" {
		return PullSnapshot{}, "", fmt.Errorf("platform returned an empty PR head")
	}
	if !snapshot.Open {
		return snapshot, StatusClosed, nil
	}
	if !strings.EqualFold(snapshot.Head, cfg.Head) {
		return snapshot, StatusHeadChanged, nil
	}
	return snapshot, "", nil
}

// mergeEvents folds freshly fetched events into the durable state, assigning
// the baseline flag and completion binding. It returns how many events were
// new or refreshed, and never drops a previously collected event.
func mergeEvents(st *State, events []Event, head string, baseline bool) int {
	if st.Seen == nil {
		st.Seen = map[string]bool{}
	}
	index := make(map[string]int, len(st.Events))
	for i := range st.Events {
		index[st.Events[i].Key()] = i
	}

	changed := 0
	for _, e := range events {
		if e.ID == "" || e.Kind == "" {
			continue
		}
		key := e.Key()

		if pos, collected := index[key]; collected {
			// The same ID may later carry new lifecycle-relevant fields (a
			// pending review submitted, or a submitted review dismissed).
			// Refresh in place instead of double-counting; the baseline flag
			// is sticky because arrival time, not content, defines it.
			prev := st.Events[pos]
			e.Baseline = prev.Baseline
			e.Binding = bindingFor(e, head, prev.Baseline)
			if prev.State == e.State && prev.CommitID == e.CommitID &&
				prev.CreatedAt == e.CreatedAt && prev.Binding == e.Binding {
				continue
			}
			st.Events[pos] = e
			changed++
			continue
		}
		e.Baseline = baseline
		e.Binding = bindingFor(e, head, baseline)
		st.Seen[key] = true
		index[key] = len(st.Events)
		st.Events = append(st.Events, e)
		changed++
	}
	return changed
}

// bindingFor classifies an event's completion evidence against the observed
// head. Only a submitted review carries a commit binding. A top-level comment
// first observed after the baseline is temporal, stable-head evidence and is
// labeled as such: it is never claimed to be commit-bound. Inline comments and
// pre-existing top-level comments bind nothing.
func bindingFor(e Event, head string, baseline bool) Binding {
	switch e.Kind {
	case EventKindReview:
		// Only a *submitted* review on the current head is commit-bound
		// evidence. A pending or dismissed review shares the commit field
		// but is not a response, so it must not be labeled commit-bound.
		if ReviewStateCountsAsSubmission(e.State) && e.CommitID != "" && strings.EqualFold(e.CommitID, head) {
			return BindingCommit
		}
		return BindingNone
	case EventKindIssueComment:
		if e.Baseline || baseline {
			return BindingNone
		}
		return BindingStableHead
	default:
		return BindingNone
	}
}

// respondsTo reports whether an event is a usable response from author for the
// current head. Inline comments are deliberately excluded: collecting one is
// not proof that a multi-comment review finished.
func respondsTo(e Event, author, head string) bool {
	if !strings.EqualFold(e.Author, author) {
		return false
	}
	switch e.Kind {
	case EventKindReview:
		// A submitted review bound to the current head counts whether or not
		// it predates the baseline: the commit binding is the proof, not
		// arrival time. Pending and dismissed reviews never count.
		return ReviewStateCountsAsSubmission(e.State) && e.Binding == BindingCommit
	case EventKindIssueComment:
		return e.Binding == BindingStableHead
	default:
		return false
	}
}

// evaluate splits the selected authors into responded and outstanding.
func evaluate(st *State, selected []string) (responded, outstanding []string) {
	responded = make([]string, 0, len(selected))
	outstanding = make([]string, 0, len(selected))
	for _, author := range selected {
		hit := false
		for _, e := range st.Events {
			if respondsTo(e, author, st.Head) {
				hit = true
				break
			}
		}
		if hit {
			responded = append(responded, author)
		} else {
			outstanding = append(outstanding, author)
		}
	}
	return responded, outstanding
}

// fillEvaluation refreshes the author partition and collected events on the
// result from the durable state.
func fillEvaluation(result *LifecycleResult, st *State) {
	responded, outstanding := evaluate(st, st.Authors)
	result.Responded = responded
	result.Outstanding = outstanding

	events := make([]CollectedEvent, 0, len(st.Events))
	baseline := 0
	for _, e := range st.Events {
		if e.Baseline {
			baseline++
		}
		events = append(events, CollectedEvent{
			ID:        e.ID,
			Kind:      e.Kind,
			Author:    e.Author,
			State:     e.State,
			CommitID:  e.CommitID,
			CreatedAt: e.CreatedAt,
			Path:      e.Path,
			Line:      e.Line,
			URL:       e.URL,
			Binding:   e.Binding,
			Baseline:  e.Baseline,
			Body:      e.Body,
		})
	}
	result.Events = events
	result.Baseline = baseline
	result.Polls = st.Polls
}

// readySelected reports readiness for the accumulated evaluation. It exists so
// the loop and tests share one rule.
func readySelected(waitFor string, responded, selected []string) bool {
	if waitFor == WaitForAny {
		return len(responded) > 0
	}
	return len(responded) == len(selected)
}

// finishError converts a fatal error into a result plus the error, so the CLI
// can still print a machine-readable status. The cursor is intentionally left
// untouched: a state or setup error must never reset collected evidence.
func finishError(result LifecycleResult, cfg LifecycleConfig, start time.Time, err error) (LifecycleResult, error) {
	result.Status = StatusError
	result.Error = err.Error()
	result.ElapsedMS = cfg.Now().Sub(start).Milliseconds()
	return result, err
}

// maybeSleep waits for the poll interval, clamped to the remaining budget. It
// returns false when the deadline has passed or the sleep was interrupted.
func maybeSleep(cfg LifecycleConfig, d time.Duration, deadline time.Time) bool {
	remaining := deadline.Sub(cfg.Now())
	if remaining <= 0 {
		return false
	}
	if d > remaining {
		d = remaining
	}
	if d <= 0 {
		return false
	}
	return cfg.Sleep(d)
}

// sleepReal is the production poll wait.
func sleepReal(d time.Duration) bool {
	time.Sleep(d)
	return true
}

// saveQuietly persists best-effort state on a terminal path. A round that
// never completed its baseline is not persisted at all: a half-initialized
// cursor would be unresumable, and leaving the path absent lets the next run
// initialize cleanly.
func saveQuietly(cfg LifecycleConfig, st *State) {
	if st == nil || !st.BaselineComplete {
		return
	}
	if err := SaveState(cfg.StatePath, st, cfg.Now()); err != nil {
		cfg.Log("- Warning: could not persist state: %v", err)
	}
}

// countBoundReviews counts reviews already bound to the observed head.
func countBoundReviews(st *State) int {
	n := 0
	for _, e := range st.Events {
		if e.Kind == EventKindReview && e.Binding == BindingCommit {
			n++
		}
	}
	return n
}

// snapshotState renders a snapshot's open/merged state for logs.
func snapshotState(s PullSnapshot) string {
	switch {
	case s.Merged:
		return "merged"
	case !s.Open:
		return "closed"
	default:
		return "open"
	}
}

// shortSHA abbreviates a SHA for logs; empty stays empty.
func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// authorLabel describes the selected author set for logs.
func authorLabel(cfg LifecycleConfig) string {
	if len(cfg.Authors) == 0 {
		return "any reviewer"
	}
	names := append([]string(nil), cfg.Authors...)
	sort.Strings(names)
	return fmt.Sprintf("%s from %s", cfg.WaitFor, strings.Join(names, ", "))
}

// IsStateError reports whether err is a durable-state contract violation,
// which the CLI must never retry.
func IsStateError(err error) bool {
	var se *StateError
	return errors.As(err, &se)
}

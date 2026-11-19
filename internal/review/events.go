package review

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EventKind classifies a lifecycle event collected from a pull request.
type EventKind string

const (
	// EventKindReview is a submitted (or pending/dismissed) review object.
	// Its State carries the platform review state and its CommitID the commit
	// the review was submitted against, when the platform exposes one.
	EventKindReview EventKind = "review"
	// EventKindReviewComment is a root inline review comment. Inline comments
	// are evidence only: they never complete a reviewer.
	EventKindReviewComment EventKind = "review_comment"
	// EventKindIssueComment is a top-level conversation comment on the PR.
	EventKindIssueComment EventKind = "issue_comment"
)

// Binding records how an event ties to the PR head observed while collecting.
type Binding string

const (
	// BindingCommit means the event carries a commit equal to the observed
	// head (GitHub and Forgejo submitted reviews).
	BindingCommit Binding = "commit"
	// BindingStableHead means the event is a top-level comment first observed
	// as new between two identical head snapshots. It is temporal evidence: it
	// is never claimed to be commit-bound.
	BindingStableHead Binding = "stable-head"
	// BindingNone means the event has no current-head binding: stale review
	// commits, inline comments, and anything captured in the pre-wait baseline.
	BindingNone Binding = "none"
)

// Review states normalized across platforms.
const (
	ReviewStatePending   = "PENDING"
	ReviewStateDismissed = "DISMISSED"
)

// PullSnapshot is an authoritative point-in-time read of PR head and open
// state. Callers compare Head before and after an event fetch so stale events
// are never credited to a new head.
type PullSnapshot struct {
	Platform   Platform
	Owner      string
	Repo       string
	PR         int
	Head       string
	Open       bool
	Merged     bool
	ObservedAt time.Time
}

// Event is one typed thing that happened on the PR, as collected by a
// platform backend. Body content is stored verbatim and never executed.
type Event struct {
	ID        string    `json:"id"`
	Kind      EventKind `json:"kind"`
	Author    string    `json:"author"`
	State     string    `json:"state,omitempty"`
	CommitID  string    `json:"commitId,omitempty"`
	CreatedAt string    `json:"createdAt,omitempty"`
	Body      string    `json:"body,omitempty"`
	Path      string    `json:"path,omitempty"`
	Line      int       `json:"line,omitempty"`
	URL       string    `json:"url,omitempty"`
	// Binding is assigned by the lifecycle state machine when the event is
	// merged, not by the backend, because it depends on the observed head and
	// on whether the event arrived after the baseline.
	Binding  Binding `json:"binding"`
	Baseline bool    `json:"baseline"`
}

// Key is the cross-kind dedupe key. Kinds are prefixed because Forgejo review
// and comment IDs share one numeric sequence.
func (e Event) Key() string { return string(e.Kind) + ":" + e.ID }

// EventBackend reads authoritative PR state and typed events for one PR.
type EventBackend interface {
	// Snapshot reads head plus open/merged state.
	Snapshot(ctx context.Context) (PullSnapshot, error)
	// Events reads all reviews, root inline comments and top-level comments.
	Events(ctx context.Context) ([]Event, error)
}

// NewEventBackend builds the platform backend used by lifecycle watching.
func NewEventBackend(platform Platform, owner, repo string, pr int) (EventBackend, error) {
	switch platform {
	case PlatformGitHub:
		return githubEventBackend{owner: owner, repo: repo, pr: pr}, nil
	case PlatformForgejo:
		return forgejoEventBackend{owner: owner, repo: repo, pr: pr}, nil
	default:
		return nil, fmt.Errorf("unsupported platform %q", platform)
	}
}

// NormalizeReviewState uppercases a platform review state so GitHub and
// Forgejo values compare uniformly.
func NormalizeReviewState(state string) string {
	return strings.ToUpper(strings.TrimSpace(state))
}

// ReviewStateCountsAsSubmission reports whether a review state represents a
// review the author actually submitted. Pending (never submitted) and
// dismissed reviews never count; any submitted state (approved,
// changes-requested, commented, or a platform-specific submitted state) does.
func ReviewStateCountsAsSubmission(state string) bool {
	switch NormalizeReviewState(state) {
	case "", ReviewStatePending, ReviewStateDismissed:
		return false
	default:
		return true
	}
}

// NormalizeAuthors trims logins, drops empties, and removes case-insensitive
// duplicates while preserving first-seen order. It is applied to both the
// selected-author CSV and the state file so a repeated login cannot inflate the
// completion denominator.
func NormalizeAuthors(authors []string) []string {
	out := make([]string, 0, len(authors))
	seen := make(map[string]bool, len(authors))
	for _, a := range authors {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		key := strings.ToLower(a)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}

// kindRank fixes the reporting order of event kinds: reviews first (they carry
// completion evidence), then inline review comments, then top-level comments.
func kindRank(k EventKind) int {
	switch k {
	case EventKindReview:
		return 0
	case EventKindReviewComment:
		return 1
	case EventKindIssueComment:
		return 2
	default:
		return 3
	}
}

// sortEvents orders events deterministically: by kind rank, then numerically by
// ID where both IDs are numeric (Forgejo), then lexicographically.
func sortEvents(events []Event) {
	sort.SliceStable(events, func(i, j int) bool {
		ri, rj := kindRank(events[i].Kind), kindRank(events[j].Kind)
		if ri != rj {
			return ri < rj
		}
		return compareEventIDs(events[i].ID, events[j].ID) < 0
	})
}

// dedupeEvents drops repeated keys, keeping the first occurrence. Forgejo can
// return the same inline comment under more than one review ID, and a duplicate
// must never inflate evidence counts.
func dedupeEvents(events []Event) []Event {
	if len(events) == 0 {
		return events
	}
	out := make([]Event, 0, len(events))
	seen := make(map[string]bool, len(events))
	for _, e := range events {
		key := e.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out
}

// compareEventIDs compares two platform IDs numerically when possible.
func compareEventIDs(a, b string) int {
	ai, aErr := strconv.ParseInt(a, 10, 64)
	bi, bErr := strconv.ParseInt(b, 10, 64)
	switch {
	case aErr == nil && bErr == nil:
		switch {
		case ai < bi:
			return -1
		case ai > bi:
			return 1
		default:
			return 0
		}
	case aErr == nil:
		return -1
	case bErr == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

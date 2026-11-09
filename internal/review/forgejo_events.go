package review

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// forgejoEventHTTPClient is the client used for bounded lifecycle requests. It
// is a package-level seam so tests can substitute transports.
var forgejoEventHTTPClient = &http.Client{}

// forgejoEventGet performs a bounded GET against the Forgejo API and returns
// the raw body. A request that exceeds timeout is reported as an explicit
// timeout error, never as an empty result.
func forgejoEventGet(ctx context.Context, endpoint string, timeout time.Duration) (string, error) {
	baseURL, token := getForgejoEnv()
	if baseURL == "" || token == "" {
		return "", fmt.Errorf("FORGEJO_URL and FORGEJO_TOKEN must be set for Forgejo repos")
	}
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}

	endpoint, query, _ := strings.Cut(endpoint, "?")
	u, err := url.JoinPath(baseURL, "/api/v1/", endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid Forgejo endpoint: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("build forgejo request: %w", err)
	}
	req.URL.RawQuery = query
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "token "+token)

	client := *forgejoEventHTTPClient
	client.Timeout = timeout
	resp, err := client.Do(req)
	if err != nil {
		if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
			return "", fmt.Errorf("forgejo request timed out after %s: %w", timeout, err)
		}
		return "", fmt.Errorf("forgejo API request failed: %w", err)
	}
	defer resp.Body.Close()

	const maxResponseBytes = 32 << 20 // 32 MiB
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("read forgejo response: %w", err)
	}
	if len(payload) > maxResponseBytes {
		return "", fmt.Errorf("forgejo response exceeded %d bytes", maxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e errResp
		msg := strings.TrimSpace(string(payload))
		if json.Unmarshal(payload, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		return "", fmt.Errorf("forgejo API error (%d): %s", resp.StatusCode, msg)
	}
	return string(payload), nil
}

// forgejoEventBackend implements EventBackend against the Forgejo REST API.
type forgejoEventBackend struct {
	owner   string
	repo    string
	pr      int
	timeout time.Duration
	getFn   func(ctx context.Context, endpoint string, timeout time.Duration) (string, error)
}

// get executes a bounded Forgejo GET through the test seam or package default.
func (b forgejoEventBackend) get(ctx context.Context, endpoint string) (string, error) {
	if err := ctx.Err(); err != nil {
		// The round context ended before this request even started: mark a
		// round-deadline abort, distinct from a request-local timeout.
		return "", fmt.Errorf("%w: %w", err, errRoundDeadline)
	}
	timeout := b.timeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	var raw string
	var err error
	if b.getFn != nil {
		raw, err = b.getFn(ctx, endpoint, timeout)
	} else {
		raw, err = forgejoEventGet(ctx, endpoint, timeout)
	}
	// The request timeout is enforced by the client, not a context deadline,
	// so any context error here is the round ending mid-request, not the
	// request-local budget.
	if err != nil && ctx.Err() != nil {
		return "", fmt.Errorf("%w: %w", err, errRoundDeadline)
	}
	return raw, err
}

// forgejoEventPR is the subset of a Forgejo pull request used for snapshots.
type forgejoEventPR struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// Snapshot returns the authoritative PR head and open/merged state.
func (b forgejoEventBackend) Snapshot(ctx context.Context) (PullSnapshot, error) {
	raw, err := b.get(ctx, fmt.Sprintf("repos/%s/%s/pulls/%d", url.PathEscape(b.owner), url.PathEscape(b.repo), b.pr))
	if err != nil {
		return PullSnapshot{}, err
	}
	var pr forgejoEventPR
	if err := json.Unmarshal([]byte(raw), &pr); err != nil {
		return PullSnapshot{}, fmt.Errorf("parse Forgejo pull request: %w", err)
	}
	if pr.Head.SHA == "" {
		return PullSnapshot{}, fmt.Errorf("pull request head is empty in Forgejo response")
	}
	return PullSnapshot{
		Platform:   PlatformForgejo,
		Owner:      b.owner,
		Repo:       b.repo,
		PR:         b.pr,
		Head:       pr.Head.SHA,
		Open:       strings.EqualFold(pr.State, "open"),
		Merged:     pr.Merged,
		ObservedAt: time.Now().UTC(),
	}, nil
}

// forgejoEventReview is a review with the lifecycle-relevant fields the legacy
// fetcher discards.
type forgejoEventReview struct {
	ID          json.Number `json:"id"`
	State       string      `json:"state"`
	Dismissed   bool        `json:"dismissed"`
	CommitID    string      `json:"commit_id"`
	SubmittedAt *string     `json:"submitted_at"`
	Body        string      `json:"body"`
	HTMLURL     string      `json:"html_url"`
	User        *struct {
		Login string `json:"login"`
	} `json:"user"`
}

// forgejoEventComment is a review comment or an issue comment.
type forgejoEventComment struct {
	ID        json.Number `json:"id"`
	Body      string      `json:"body"`
	CreatedAt *string     `json:"created_at"`
	HTMLURL   string      `json:"html_url"`
	Path      *string     `json:"path"`
	Position  int         `json:"position"`
	OrigPos   int         `json:"original_position"`
	CommitID  string      `json:"commit_id"`
	User      *struct {
		Login string `json:"login"`
	} `json:"user"`
}

// Events reads reviews, inline review comments and top-level issue comments.
func (b forgejoEventBackend) Events(ctx context.Context) ([]Event, error) {
	reviews, reviewIDs, err := b.reviews(ctx)
	if err != nil {
		return nil, err
	}
	inline, err := b.reviewComments(ctx, reviewIDs)
	if err != nil {
		return nil, err
	}
	issueComments, err := b.issueComments(ctx)
	if err != nil {
		return nil, err
	}
	all := make([]Event, 0, len(reviews)+len(inline)+len(issueComments))
	all = append(all, reviews...)
	all = append(all, inline...)
	all = append(all, issueComments...)
	all = dedupeEvents(all)
	sortEvents(all)
	return all, nil
}

// reviews paginates the PR review list and returns both events and the review
// IDs needed to enumerate inline comments.
func (b forgejoEventBackend) reviews(ctx context.Context) ([]Event, []string, error) {
	var out []Event
	var ids []string
	for page := 1; page <= 50; page++ {
		endpoint := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews?page=%d&limit=%d",
			url.PathEscape(b.owner), url.PathEscape(b.repo), b.pr, page, eventPageSize)
		raw, err := b.get(ctx, endpoint)
		if err != nil {
			return nil, nil, err
		}
		var reviews []forgejoEventReview
		if err := json.Unmarshal([]byte(raw), &reviews); err != nil {
			return nil, nil, fmt.Errorf("parse Forgejo reviews: %w", err)
		}
		for _, r := range reviews {
			id := r.ID.String()
			if id == "" {
				continue
			}
			ids = append(ids, id)
			author := ""
			if r.User != nil {
				author = r.User.Login
			}
			state := NormalizeReviewState(r.State)
			if r.Dismissed {
				// Dismissal supersedes the recorded state: a dismissed review
				// must never complete a reviewer.
				state = ReviewStateDismissed
			}
			created := ""
			if r.SubmittedAt != nil {
				created = *r.SubmittedAt
			}
			out = append(out, Event{
				ID:        id,
				Kind:      EventKindReview,
				Author:    author,
				State:     state,
				CommitID:  r.CommitID,
				CreatedAt: created,
				Body:      r.Body,
				URL:       r.HTMLURL,
				Binding:   BindingNone,
			})
		}
		if len(reviews) == 0 {
			return out, ids, nil
		}
	}
	return nil, nil, fmt.Errorf("forgejo reviews pagination exceeded 50 pages; observation incomplete")
}

// reviewComments enumerates inline comments per review. Root-only filtering is
// applied by the caller's event contract; Forgejo has no reply linkage on this
// instance, so every inline comment is treated as its own root event.
func (b forgejoEventBackend) reviewComments(ctx context.Context, reviewIDs []string) ([]Event, error) {
	var out []Event
	for _, reviewID := range reviewIDs {
		endpoint := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews/%s/comments",
			url.PathEscape(b.owner), url.PathEscape(b.repo), b.pr, url.PathEscape(reviewID))
		raw, err := b.get(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		var comments []forgejoEventComment
		if err := json.Unmarshal([]byte(raw), &comments); err != nil {
			return nil, fmt.Errorf("parse Forgejo review comments: %w", err)
		}
		for _, c := range comments {
			id := c.ID.String()
			if id == "" {
				continue
			}
			author := ""
			if c.User != nil {
				author = c.User.Login
			}
			path := ""
			if c.Path != nil {
				path = *c.Path
			}
			line := c.Position
			if line == 0 {
				line = c.OrigPos
			}
			created := ""
			if c.CreatedAt != nil {
				created = *c.CreatedAt
			}
			out = append(out, Event{
				ID:        id,
				Kind:      EventKindReviewComment,
				Author:    author,
				CommitID:  c.CommitID,
				CreatedAt: created,
				Body:      c.Body,
				Path:      path,
				Line:      line,
				URL:       c.HTMLURL,
				Binding:   BindingNone,
			})
		}
	}
	return out, nil
}

// issueComments paginates the PR's top-level conversation comments.
func (b forgejoEventBackend) issueComments(ctx context.Context) ([]Event, error) {
	var out []Event
	for page := 1; page <= 50; page++ {
		endpoint := fmt.Sprintf("repos/%s/%s/issues/%d/comments?page=%d&limit=%d",
			url.PathEscape(b.owner), url.PathEscape(b.repo), b.pr, page, eventPageSize)
		raw, err := b.get(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		var comments []forgejoEventComment
		if err := json.Unmarshal([]byte(raw), &comments); err != nil {
			return nil, fmt.Errorf("parse Forgejo issue comments: %w", err)
		}
		for _, c := range comments {
			id := c.ID.String()
			if id == "" {
				continue
			}
			author := ""
			if c.User != nil {
				author = c.User.Login
			}
			created := ""
			if c.CreatedAt != nil {
				created = *c.CreatedAt
			}
			out = append(out, Event{
				ID:        id,
				Kind:      EventKindIssueComment,
				Author:    author,
				CreatedAt: created,
				Body:      c.Body,
				URL:       c.HTMLURL,
				Binding:   BindingNone,
			})
		}
		if len(comments) == 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("forgejo issue comments pagination exceeded 50 pages; observation incomplete")
}

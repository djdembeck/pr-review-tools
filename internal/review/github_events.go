package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultRequestTimeout caps each lifecycle request; the round context can
// cancel it sooner when the remaining overall budget expires.
const DefaultRequestTimeout = 30 * time.Second

// eventPageSize is the page size used for every paginated event read.
const eventPageSize = 100

// GITHUB_EVENTS_SNAPSHOT_QUERY reads the authoritative head and state of a PR.
const GITHUB_EVENTS_SNAPSHOT_QUERY = `query($owner: String!, $repo: String!, $pr: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      headRefOid
      state
    }
  }
}`

// GITHUB_EVENTS_REVIEWS_QUERY reads one page of submitted reviews.
const GITHUB_EVENTS_REVIEWS_QUERY = `query($owner: String!, $repo: String!, $pr: Int!, $after: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      reviews(first: 100, after: $after) {
        nodes {
          id
          state
          submittedAt
          body
          url
          commit { oid }
          author { login }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

// GITHUB_EVENTS_THREAD_ROOTS_QUERY reads one page of review threads together
// with their root comment only. Replies are deliberately not collected for
// lifecycle events: they are not reviewer completion signals.
const GITHUB_EVENTS_THREAD_ROOTS_QUERY = `query($owner: String!, $repo: String!, $pr: Int!, $after: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      reviewThreads(first: 100, after: $after) {
        nodes {
          id
          comments(first: 1) {
            nodes {
              databaseId
              body
              author { login }
              path
              line
              originalLine
              createdAt
              url
            }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

// ghRunWithTimeout executes the gh CLI with a bounded request deadline. It is a
// seam so lifecycle tests can stub the CLI without shelling out.
var ghRunWithTimeout func(ctx context.Context, args []string, timeout time.Duration) (string, error) = runGhWithTimeout

// ghRunTimeoutMu protects ghRunWithTimeout from concurrent reads/writes.
var ghRunTimeoutMu sync.RWMutex

// ghRunTimeoutGuarded executes the gh CLI through the overridable seam.
func ghRunTimeoutGuarded(ctx context.Context, args []string, timeout time.Duration) (string, error) {
	ghRunTimeoutMu.RLock()
	defer ghRunTimeoutMu.RUnlock()
	return ghRunWithTimeout(ctx, args, timeout)
}

// runGhWithTimeout runs gh with an argument vector (never a shell) and returns
// trimmed stdout, mapping a deadline overrun to an explicit timeout error.
func runGhWithTimeout(ctx context.Context, args []string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.WaitDelay = 100 * time.Millisecond
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("gh request timed out after %s: %w", timeout, ctx.Err())
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("gh request canceled: %w", ctx.Err())
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh command failed: %s", msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// githubEventBackend implements EventBackend on top of the gh CLI.
type githubEventBackend struct {
	owner   string
	repo    string
	pr      int
	timeout time.Duration
	runGHFn func(ctx context.Context, args []string, timeout time.Duration) (string, error)
}

// Snapshot returns the authoritative PR head and open/merged state.
func (b githubEventBackend) Snapshot(ctx context.Context) (PullSnapshot, error) {
	raw, err := b.run(ctx, []string{
		"api", "graphql",
		"-F", "query=" + GITHUB_EVENTS_SNAPSHOT_QUERY,
		"-F", "owner=" + b.owner,
		"-F", "repo=" + b.repo,
		"-F", fmt.Sprintf("pr=%d", b.pr),
	})
	if err != nil {
		return PullSnapshot{}, err
	}
	var resp struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					HeadRefOID string `json:"headRefOid"`
					State      string `json:"state"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []graphQLError `json:"errors"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return PullSnapshot{}, fmt.Errorf("invalid GraphQL response: %w", err)
	}
	if msg := graphQLErrorMessage(resp.Errors); msg != "" {
		return PullSnapshot{}, fmt.Errorf("GraphQL error: %s", msg)
	}
	pr := resp.Data.Repository
	if pr == nil || pr.PullRequest == nil {
		return PullSnapshot{}, fmt.Errorf("no pull request data in GraphQL response")
	}
	if pr.PullRequest.HeadRefOID == "" {
		return PullSnapshot{}, fmt.Errorf("pull request head is empty in GraphQL response")
	}
	state := strings.ToUpper(pr.PullRequest.State)
	return PullSnapshot{
		Platform:   PlatformGitHub,
		Owner:      b.owner,
		Repo:       b.repo,
		PR:         b.pr,
		Head:       pr.PullRequest.HeadRefOID,
		Open:       state == "OPEN",
		Merged:     state == "MERGED",
		ObservedAt: time.Now().UTC(),
	}, nil
}

// Events reads reviews, root inline review comments and top-level issue
// comments for the PR.
func (b githubEventBackend) Events(ctx context.Context) ([]Event, error) {
	reviews, err := b.reviews(ctx)
	if err != nil {
		return nil, err
	}
	roots, err := b.threadRoots(ctx)
	if err != nil {
		return nil, err
	}
	issueComments, err := b.issueComments(ctx)
	if err != nil {
		return nil, err
	}
	all := make([]Event, 0, len(reviews)+len(roots)+len(issueComments))
	all = append(all, reviews...)
	all = append(all, roots...)
	all = append(all, issueComments...)
	all = dedupeEvents(all)
	sortEvents(all)
	return all, nil
}

// run executes one bounded gh invocation through the test seam or the package
// default.
func (b githubEventBackend) run(ctx context.Context, args []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	timeout := b.timeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	if b.runGHFn != nil {
		return b.runGHFn(ctx, args, timeout)
	}
	return ghRunTimeoutGuarded(ctx, args, timeout)
}

// reviews paginates the pullRequest.reviews connection.
func (b githubEventBackend) reviews(ctx context.Context) ([]Event, error) {
	type reviewNode struct {
		ID          string  `json:"id"`
		State       string  `json:"state"`
		SubmittedAt *string `json:"submittedAt"`
		Body        string  `json:"body"`
		URL         string  `json:"url"`
		Commit      *struct {
			OID string `json:"oid"`
		} `json:"commit"`
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	var out []Event
	cursor := ""
	for range 50 {
		args := []string{
			"api", "graphql",
			"-F", "query=" + GITHUB_EVENTS_REVIEWS_QUERY,
			"-F", "owner=" + b.owner,
			"-F", "repo=" + b.repo,
			"-F", fmt.Sprintf("pr=%d", b.pr),
		}
		if cursor != "" {
			args = append(args, "-F", "after="+cursor)
		}
		raw, err := b.run(ctx, args)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						Reviews struct {
							Nodes    []reviewNode `json:"nodes"`
							PageInfo ghPageInfo   `json:"pageInfo"`
						} `json:"reviews"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []graphQLError `json:"errors"`
		}
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			return nil, fmt.Errorf("invalid GraphQL response: %w", err)
		}
		if msg := graphQLErrorMessage(resp.Errors); msg != "" {
			return nil, fmt.Errorf("GraphQL error: %s", msg)
		}
		pr := resp.Data.Repository
		if pr == nil || pr.PullRequest == nil {
			return nil, fmt.Errorf("no pull request data in GraphQL response")
		}
		conn := pr.PullRequest.Reviews
		for _, node := range conn.Nodes {
			if node.ID == "" {
				continue
			}
			var created string
			if node.SubmittedAt != nil {
				created = *node.SubmittedAt
			}
			var commitID string
			if node.Commit != nil {
				commitID = node.Commit.OID
			}
			var author string
			if node.Author != nil {
				author = node.Author.Login
			}
			out = append(out, Event{
				ID:        node.ID,
				Kind:      EventKindReview,
				Author:    author,
				State:     NormalizeReviewState(node.State),
				CommitID:  commitID,
				CreatedAt: created,
				Body:      node.Body,
				URL:       node.URL,
				Binding:   BindingNone,
			})
		}
		if !conn.PageInfo.HasNextPage || conn.PageInfo.EndCursor == "" {
			break
		}
		cursor = conn.PageInfo.EndCursor
	}
	return out, nil
}

// threadRoots paginates review threads and keeps only each thread's root
// comment.
func (b githubEventBackend) threadRoots(ctx context.Context) ([]Event, error) {
	type rootNode struct {
		DatabaseID json.Number `json:"databaseId"`
		Body       string      `json:"body"`
		Author     *struct {
			Login string `json:"login"`
		} `json:"author"`
		Path       *string `json:"path"`
		Line       *int    `json:"line"`
		OriginalLn *int    `json:"originalLine"`
		CreatedAt  *string `json:"createdAt"`
		URL        string  `json:"url"`
	}
	type threadNode struct {
		ID       string `json:"id"`
		Comments struct {
			Nodes []rootNode `json:"nodes"`
		} `json:"comments"`
	}
	var out []Event
	cursor := ""
	for range 50 {
		args := []string{
			"api", "graphql",
			"-F", "query=" + GITHUB_EVENTS_THREAD_ROOTS_QUERY,
			"-F", "owner=" + b.owner,
			"-F", "repo=" + b.repo,
			"-F", fmt.Sprintf("pr=%d", b.pr),
		}
		if cursor != "" {
			args = append(args, "-F", "after="+cursor)
		}
		raw, err := b.run(ctx, args)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						ReviewThreads struct {
							Nodes    []threadNode `json:"nodes"`
							PageInfo ghPageInfo   `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []graphQLError `json:"errors"`
		}
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			return nil, fmt.Errorf("invalid GraphQL response: %w", err)
		}
		if msg := graphQLErrorMessage(resp.Errors); msg != "" {
			return nil, fmt.Errorf("GraphQL error: %s", msg)
		}
		pr := resp.Data.Repository
		if pr == nil || pr.PullRequest == nil {
			return nil, fmt.Errorf("no pull request data in GraphQL response")
		}
		conn := pr.PullRequest.ReviewThreads
		for _, thread := range conn.Nodes {
			if len(thread.Comments.Nodes) == 0 {
				continue
			}
			node := thread.Comments.Nodes[0]
			id := node.DatabaseID.String()
			if id == "" {
				id = thread.ID
			}
			if id == "" {
				continue
			}
			author := ""
			if node.Author != nil {
				author = node.Author.Login
			}
			path := ""
			if node.Path != nil {
				path = *node.Path
			}
			line := 0
			if node.Line != nil {
				line = *node.Line
			} else if node.OriginalLn != nil {
				line = *node.OriginalLn
			}
			created := ""
			if node.CreatedAt != nil {
				created = *node.CreatedAt
			}
			out = append(out, Event{
				ID:        id,
				Kind:      EventKindReviewComment,
				Author:    author,
				CreatedAt: created,
				Body:      node.Body,
				Path:      path,
				Line:      line,
				URL:       node.URL,
				Binding:   BindingNone,
			})
		}
		if !conn.PageInfo.HasNextPage || conn.PageInfo.EndCursor == "" {
			break
		}
		cursor = conn.PageInfo.EndCursor
	}
	return out, nil
}

// issueComments paginates the PR's top-level conversation comments through the
// REST endpoint. gh --paginate --slurp yields one array per page.
func (b githubEventBackend) issueComments(ctx context.Context) ([]Event, error) {
	endpoint := fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=%d", b.owner, b.repo, b.pr, eventPageSize)
	raw, err := b.run(ctx, []string{"api", "--paginate", "--slurp", endpoint})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	type issueComment struct {
		ID        json.Number `json:"id"`
		Body      string      `json:"body"`
		CreatedAt *string     `json:"created_at"`
		HTMLURL   string      `json:"html_url"`
		User      *struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	var pages [][]issueComment
	if err := json.Unmarshal([]byte(raw), &pages); err != nil {
		return nil, fmt.Errorf("invalid issue comment response: %w", err)
	}
	var out []Event
	for _, page := range pages {
		for _, c := range page {
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
	}
	return out, nil
}

package review

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// GITHUB_GRAPHQL fetches one page (100) of review threads, each carrying its
// first page (50) of comments, plus the pagination cursor for the next page.
const GITHUB_GRAPHQL = `query($owner: String!, $repo: String!, $pr: Int!, $after: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      reviewThreads(first: 100, after: $after) {
        nodes {
          id
          isResolved
          isOutdated
          comments(first: 50) {
            nodes {
              databaseId
              body
              author { login }
              path
              line
              originalLine
              startLine
              originalStartLine
              diffHunk
              createdAt
              replyTo { databaseId }
            }
            pageInfo { hasNextPage endCursor }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

// GITHUB_THREAD_COMMENTS_QUERY fetches one page (50) of a single review
// thread's comments, used to continue paginating threads that have more than
// 50 comments. It resolves the thread through the generic node(id:) interface
// because PullRequest has no singular reviewThread field (only reviewThreads),
// so repository.pullRequest.reviewThread is not a valid GraphQL path.
const GITHUB_THREAD_COMMENTS_QUERY = `query($threadId: ID!, $after: String) {
  node(id: $threadId) {
    ... on PullRequestReviewThread {
      comments(first: 50, after: $after) {
        nodes {
          databaseId
          body
          author { login }
          path
          line
          originalLine
          startLine
          originalStartLine
          diffHunk
          createdAt
          replyTo { databaseId }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

// ghRun executes the gh CLI. Overridable in tests to avoid shelling out.
var ghRun func(args []string) (string, error) = runGh

// ghRunMu protects ghRun from concurrent reads/writes.
var ghRunMu sync.RWMutex

// ghRunGuarded executes the gh CLI through the overridable ghRun seam.
func ghRunGuarded(args []string) (string, error) {
	ghRunMu.RLock()
	defer ghRunMu.RUnlock()
	return ghRun(args)
}

// runGh executes the gh CLI with the given args and returns trimmed stdout. A
// non-zero exit produces an error carrying stderr.
func runGh(args []string) (string, error) {
	cmd := exec.Command("gh", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = "unknown error"
		}
		return "", fmt.Errorf("gh command failed: %s", msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// FetchGitHubComments fetches all review-thread comments for the given PR via
// the gh CLI GraphQL endpoint, fully paginating both the reviewThreads
// connection (100 per page) and each thread's comments connection (50 per
// page), and normalizes the accumulated comments into RawComment values.
func FetchGitHubComments(owner, repo string, prNumber int) ([]RawComment, error) {
	var threads []ghThread
	cursor := ""
	for {
		args := []string{
			"api", "graphql",
			"-F", "query=" + GITHUB_GRAPHQL,
			"-F", "owner=" + owner,
			"-F", "repo=" + repo,
			"-F", fmt.Sprintf("pr=%d", prNumber),
		}
		if cursor != "" {
			args = append(args, "-F", "after="+cursor)
		}
		output, err := ghRunGuarded(args)
		if err != nil {
			return nil, err
		}
		var resp graphQLResponse
		if err := checkGitHubGraphQLPage(&resp, output); err != nil {
			return nil, err
		}
		page := resp.Data.Repository.PullRequest.ReviewThreads
		threads = append(threads, page.Nodes...)
		if !page.PageInfo.HasNextPage {
			break
		}
		cursor = page.PageInfo.EndCursor
	}

	// Threads may have more than 50 comments; paginate the remainder via the
	// generic node(id:) interface (PullRequest has no singular reviewThread
	// field).
	for i := range threads {
		pageInfo := threads[i].Comments.PageInfo
		for pageInfo.HasNextPage {
			args := []string{
				"api", "graphql",
				"-F", "query=" + GITHUB_THREAD_COMMENTS_QUERY,
				"-F", "threadId=" + threads[i].ID,
				"-F", "after=" + pageInfo.EndCursor,
			}
			output, err := ghRunGuarded(args)
			if err != nil {
				return nil, err
			}
			var resp graphQLThreadCommentsResponse
			if err := checkGitHubThreadCommentsPage(&resp, output); err != nil {
				return nil, err
			}
			if resp.Data.Node == nil {
				return nil, fmt.Errorf("reviewThread %s not found while paginating comments", threads[i].ID)
			}
			threads[i].Comments.Nodes = append(threads[i].Comments.Nodes, resp.Data.Node.Comments.Nodes...)
			pageInfo = resp.Data.Node.Comments.PageInfo
		}
	}

	return mapThreadsToRawComments(threads), nil
}

// checkGitHubGraphQLPage unmarshals a GITHUB_GRAPHQL response page and reports
// GraphQL errors or a missing data envelope.
func checkGitHubGraphQLPage(resp *graphQLResponse, raw string) error {
	if err := json.Unmarshal([]byte(raw), resp); err != nil {
		return fmt.Errorf("invalid GraphQL response: %w", err)
	}
	if msg := graphQLErrorMessage(resp.Errors); msg != "" {
		return fmt.Errorf("GraphQL error: %s", msg)
	}
	if resp.Data == nil || resp.Data.Repository == nil || resp.Data.Repository.PullRequest == nil {
		return fmt.Errorf("no pull request data in GraphQL response")
	}
	return nil
}

// checkGitHubThreadCommentsPage unmarshals a GITHUB_THREAD_COMMENTS_QUERY
// response page and reports GraphQL errors or a missing data envelope.
func checkGitHubThreadCommentsPage(resp *graphQLThreadCommentsResponse, raw string) error {
	if err := json.Unmarshal([]byte(raw), resp); err != nil {
		return fmt.Errorf("invalid GraphQL response: %w", err)
	}
	if msg := graphQLErrorMessage(resp.Errors); msg != "" {
		return fmt.Errorf("GraphQL error: %s", msg)
	}
	if resp.Data.Node == nil {
		return fmt.Errorf("no review thread data in GraphQL response")
	}
	return nil
}

// graphQLErrorMessage returns the first GraphQL error's message, "unknown"
// when the first error has no message, or "" when there are no errors.
func graphQLErrorMessage(errors []graphQLError) string {
	if len(errors) == 0 {
		return ""
	}
	if msg := errors[0].Message; msg != "" {
		return msg
	}
	return "unknown"
}

// graphQLThreadCommentsResponse is the response for GITHUB_THREAD_COMMENTS_QUERY.
type graphQLThreadCommentsResponse struct {
	Data struct {
		Node *ghThread `json:"node"`
	} `json:"data"`
	Errors []graphQLError `json:"errors"`
}

// parseGitHubGraphQLResponse decodes a single GraphQL page and maps
// threads+comments into RawComment values. Mirrors the TS parsing logic
// including the line ?? originalLine and startLine ?? originalStartLine
// fallbacks.
func parseGitHubGraphQLResponse(raw string) ([]RawComment, error) {
	var resp graphQLResponse
	if err := checkGitHubGraphQLPage(&resp, raw); err != nil {
		return nil, err
	}
	return mapThreadsToRawComments(resp.Data.Repository.PullRequest.ReviewThreads.Nodes), nil
}

// mapThreadsToRawComments maps fully-populated threads (all comment pages
// accumulated) into RawComment values.
func mapThreadsToRawComments(threads []ghThread) []RawComment {
	comments := make([]RawComment, 0, len(threads))
	for _, thread := range threads {
		nodes := thread.Comments.Nodes
		replyCount := len(nodes) - 1
		if replyCount < 0 {
			replyCount = 0
		}

		for _, node := range nodes {
			author := ""
			if node.Author != nil {
				author = node.Author.Login
			}

			databaseID := ""
			if node.DatabaseID != "" {
				databaseID = node.DatabaseID.String()
			}
			if databaseID == "" {
				databaseID = thread.ID
			}

			var replyToID *string
			isRoot := true
			if node.ReplyTo != nil && node.ReplyTo.DatabaseID != "" {
				s := node.ReplyTo.DatabaseID.String()
				replyToID = &s
				isRoot = false
			}

			line := node.Line
			if line == nil {
				line = node.OriginalLine
			}
			startLine := node.StartLine
			if startLine == nil {
				startLine = node.OriginalStartLine
			}

			threadReplies := 0
			if isRoot {
				threadReplies = replyCount
			}

			threadID := thread.ID
			comments = append(comments, RawComment{
				ID:            databaseID,
				Body:          node.Body,
				Path:          node.Path,
				Line:          line,
				StartLine:     startLine,
				DiffHunk:      node.DiffHunk,
				Author:        author,
				CreatedAt:     node.CreatedAt,
				IsResolved:    thread.IsResolved,
				IsOutdated:    thread.IsOutdated,
				ReplyToID:     replyToID,
				ThreadID:      &threadID,
				ThreadReplies: threadReplies,
			})
		}
	}

	return comments
}

// PostGitHubReply posts a reply to a review comment via the gh CLI REST API.
// When dryRun is true, no API call is made and the intended body is printed to
// stderr by the caller flow.
func PostGitHubReply(owner, repo string, prNumber int, commentID, body string, dryRun bool) (ReplyResult, error) {
	if dryRun {
		return ReplyResult{Success: true}, nil
	}
	output, err := ghRunGuarded([]string{
		"api", "-X", "POST",
		fmt.Sprintf("repos/%s/%s/pulls/%d/comments/%s/replies", owner, repo, prNumber, commentID),
		"-f", "body=" + body,
	})
	if err != nil {
		return ReplyResult{Success: false, Error: err.Error()}, nil
	}
	var resp struct {
		HTMLURL string `json:"html_url"`
	}
	if jsonErr := json.Unmarshal([]byte(output), &resp); jsonErr == nil && resp.HTMLURL != "" {
		return ReplyResult{Success: true, ReplyURL: resp.HTMLURL}, nil
	}
	return ReplyResult{Success: true}, nil
}

// DetectGitHubBotName queries the PR's review threads for the first Mira author
// and returns the login with a trailing "[bot]" suffix stripped. It paginates
// the reviewThreads connection (50 per page) so a bot comment on a later page
// is still found; it stops early at the first Mira-authored thread, so a
// match on the first page never triggers further fetches. Falls back to
// "miracodeai-bot" when no Mira-authored comment is found or the query fails.
func DetectGitHubBotName(owner, repo string, prNumber int) (string, error) {
	const query = `query($owner: String!, $repo: String!, $pr: Int!, $after: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      reviewThreads(first: 50, after: $after) {
        nodes {
          comments(first: 1) {
            nodes {
              author { login }
            }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`
	cursor := ""
	for {
		args := []string{
			"api", "graphql",
			"-F", "query=" + query,
			"-F", "owner=" + owner,
			"-F", "repo=" + repo,
			"-F", fmt.Sprintf("pr=%d", prNumber),
		}
		if cursor != "" {
			args = append(args, "-F", "after="+cursor)
		}
		output, err := ghRunGuarded(args)
		if err != nil {
			return "miracodeai-bot", nil
		}
		var resp graphQLResponse
		if err := json.Unmarshal([]byte(output), &resp); err != nil {
			return "miracodeai-bot", nil
		}
		if resp.Data == nil || resp.Data.Repository == nil || resp.Data.Repository.PullRequest == nil {
			return "miracodeai-bot", nil
		}
		for _, thread := range resp.Data.Repository.PullRequest.ReviewThreads.Nodes {
			if len(thread.Comments.Nodes) == 0 {
				continue
			}
			author := thread.Comments.Nodes[0].Author
			if author != nil && IsMiraComment(author.Login) {
				return strings.TrimSuffix(author.Login, "[bot]"), nil
			}
		}
		pageInfo := resp.Data.Repository.PullRequest.ReviewThreads.PageInfo
		if !pageInfo.HasNextPage {
			break
		}
		cursor = pageInfo.EndCursor
	}
	return "miracodeai-bot", nil
}

// GITHUB_RESOLVE_MUTATION resolves a single review thread by its GraphQL node
// ID.
const GITHUB_RESOLVE_MUTATION = `mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
  }
}`

// FindThreadID returns the review-thread GraphQL node ID containing the given
// comment ID (root or reply). Returns ok=false when no comment matches.
func FindThreadID(comments []RawComment, commentID string) (string, bool) {
	for _, c := range comments {
		if c.ID == commentID && c.ThreadID != nil {
			return *c.ThreadID, true
		}
	}
	return "", false
}

// ResolveGitHubThread resolves the review thread containing threadID via the
// gh CLI GraphQL endpoint. When dryRun is true, no API call is made. GraphQL
// errors are reported as a failed ReplyResult with a nil error, mirroring
// PostGitHubReply.
func ResolveGitHubThread(owner, repo string, prNumber int, threadID string, dryRun bool) (ReplyResult, error) {
	if dryRun {
		return ReplyResult{Success: true}, nil
	}
	output, err := ghRunGuarded([]string{
		"api", "graphql",
		"-f", "query=" + GITHUB_RESOLVE_MUTATION,
		"-F", "threadId=" + threadID,
	})
	if err != nil {
		return ReplyResult{Success: false, Error: err.Error()}, nil
	}
	var resp struct {
		Data struct {
			ResolveReviewThread struct {
				Thread struct {
					ID         string `json:"id"`
					IsResolved bool   `json:"isResolved"`
				} `json:"thread"`
			} `json:"resolveReviewThread"`
		} `json:"data"`
		Errors []graphQLError `json:"errors"`
	}
	if jsonErr := json.Unmarshal([]byte(output), &resp); jsonErr != nil {
		return ReplyResult{Success: false, Error: fmt.Sprintf("invalid GraphQL response: %v", jsonErr)}, nil
	}
	if len(resp.Errors) > 0 {
		msg := resp.Errors[0].Message
		if msg == "" {
			msg = "unknown"
		}
		return ReplyResult{Success: false, Error: "GraphQL error: " + msg}, nil
	}
	if !resp.Data.ResolveReviewThread.Thread.IsResolved {
		return ReplyResult{Success: false, Error: "resolve mutation returned isResolved=false"}, nil
	}
	return ReplyResult{Success: true}, nil
}

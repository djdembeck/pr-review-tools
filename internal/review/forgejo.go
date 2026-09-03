package review

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

// forgejoEnv reads FORGEJO_URL and FORGEJO_TOKEN. Overridable in tests to avoid
// data races on os.Setenv/os.Getenv when tests run concurrently.
var forgejoEnv func() (baseURL, token string) = func() (string, string) {
	return os.Getenv("FORGEJO_URL"), os.Getenv("FORGEJO_TOKEN")
}

// forgejoEnvMu protects forgejoEnv from concurrent reads/writes.
var forgejoEnvMu sync.RWMutex

// getForgejoEnv returns the current Forgejo base URL and token.
func getForgejoEnv() (string, string) {
	forgejoEnvMu.RLock()
	defer forgejoEnvMu.RUnlock()
	return forgejoEnv()
}

// forgejoRequest performs an HTTP request against the Forgejo API using the
// FORGEJO_URL and FORGEJO_TOKEN env vars. method is the HTTP verb, endpoint is
// the path after /api/v1/. body may be nil for GET requests.
func forgejoRequest(method, endpoint string, body []byte) (string, error) {
	baseURL, token := getForgejoEnv()
	if baseURL == "" || token == "" {
		return "", fmt.Errorf("FORGEJO_URL and FORGEJO_TOKEN must be set for Forgejo repos")
	}

	u, err := url.JoinPath(baseURL, "/api/v1/", endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid Forgejo endpoint: %w", err)
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u, reader)
	if err != nil {
		return "", fmt.Errorf("build forgejo request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "token "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("forgejo API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read forgejo response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e errResp
		msg := strings.TrimSpace(string(respBody))
		if json.Unmarshal(respBody, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		return "", fmt.Errorf("forgejo API error (%d): %s", resp.StatusCode, msg)
	}
	return string(respBody), nil
}

// forgejoGet is a convenience GET wrapper.
func forgejoGet(endpoint string) (string, error) {
	return forgejoRequest(http.MethodGet, endpoint, nil)
}

// forgejoPost is a convenience POST wrapper.
func forgejoPost(endpoint string, body []byte) (string, error) {
	return forgejoRequest(http.MethodPost, endpoint, body)
}

// FetchForgejoComments fetches all review comments for the given PR by
// iterating over reviews and their comment subsets, normalizing each into a
// RawComment.
func FetchForgejoComments(owner, repo string, prNumber int) ([]RawComment, error) {
	reviewsRaw, err := forgejoGet(fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", owner, repo, prNumber))
	if err != nil {
		return nil, err
	}
	var reviews []forgejoReview
	if err := json.Unmarshal([]byte(reviewsRaw), &reviews); err != nil {
		return nil, fmt.Errorf("parse Forgejo reviews: %w", err)
	}

	allComments := make([]RawComment, 0)
	for _, review := range reviews {
		reviewID := review.ID.String()
		if reviewID == "" {
			continue
		}
		commentsRaw, err := forgejoGet(fmt.Sprintf("repos/%s/%s/pulls/%d/reviews/%s/comments", owner, repo, prNumber, reviewID))
		if err != nil {
			return nil, err
		}
		var comments []forgejoComment
		if err := json.Unmarshal([]byte(commentsRaw), &comments); err != nil {
			return nil, fmt.Errorf("parse Forgejo review comments: %w", err)
		}
		for _, c := range comments {
			diffHunk := c.DiffHunk
			if diffHunk == nil {
				diffHunk = c.Diff
			}
			var replyToID *string
			if c.InReplyToID != nil {
				s := fmt.Sprintf("%d", *c.InReplyToID)
				replyToID = &s
			}
			id := c.ID.String()
			createdAt := c.CreatedAt
			allComments = append(allComments, RawComment{
				ID:         id,
				Body:       c.Body,
				Path:       c.Path,
				Line:       c.Line,
				StartLine:  c.StartLine,
				DiffHunk:   diffHunk,
				Author:     c.User.Login,
				CreatedAt:  createdAt,
				IsResolved: c.Resolver != nil,
				IsOutdated: false,
				ReplyToID:  replyToID,
			})
		}
	}
	return allComments, nil
}

// findForgejoReviewIDForComment returns the review ID and the matched comment
// for the given comment ID by iterating over the PR's reviews.
func findForgejoReviewIDForComment(owner, repo string, prNumber int, commentID string) (string, forgejoComment, error) {
	reviewsRaw, err := forgejoGet(fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", owner, repo, prNumber))
	if err != nil {
		return "", forgejoComment{}, err
	}
	var reviews []forgejoReview
	if err := json.Unmarshal([]byte(reviewsRaw), &reviews); err != nil {
		return "", forgejoComment{}, fmt.Errorf("parse Forgejo reviews: %w", err)
	}

	for _, review := range reviews {
		reviewID := review.ID.String()
		if reviewID == "" {
			continue
		}
		commentsRaw, err := forgejoGet(fmt.Sprintf("repos/%s/%s/pulls/%d/reviews/%s/comments", owner, repo, prNumber, reviewID))
		if err != nil {
			continue
		}
		var comments []forgejoComment
		if err := json.Unmarshal([]byte(commentsRaw), &comments); err != nil {
			continue
		}
		for _, c := range comments {
			if c.ID.String() == commentID {
				return reviewID, c, nil
			}
		}
	}
	return "", forgejoComment{}, fmt.Errorf("comment %s not found in any review on PR %d", commentID, prNumber)
}

// forgejoTryReply attempts to post a reply to a Forgejo review comment.
// It tries strategies in order:
//  1. GitHub-compatible /comments/{id}/replies endpoint (threaded, Gitea 1.24+)
//  2. /reviews/{id}/comments with path+position (threaded, current fix)
//  3. /issues/{pr}/comments as issue comment (fallback for summary comments)
func forgejoTryReply(owner, repo string, prNumber int, commentID, body, reviewID string, parent forgejoComment) (ReplyResult, error) {
	// 1. Try the GitHub-compatible reply endpoint (Gitea 1.24+)
	payload, _ := json.Marshal(map[string]string{"body": body})
	output, err := forgejoPost(fmt.Sprintf("repos/%s/%s/pulls/%d/comments/%s/replies", owner, repo, prNumber, commentID), payload)
	if err == nil {
		return parseForgejoReplyResponse(output)
	}

	// 2. Try threaded review comment with path+position
	// Fallback: map Line/StartLine to Position/OriginalPosition if the API didn't return them
	pos := parent.Position
	origPos := parent.OriginalPosition
	if pos == 0 && parent.Line != nil {
		pos = *parent.Line
	}
	// Do not fall back to StartLine for old_position:
	// start_line is a new-file range start, not an old-file line.
	if reviewID != "" && parent.Path != nil && *parent.Path != "" && (pos > 0 || origPos > 0) {
		reviewPayload := map[string]any{"body": body, "path": *parent.Path}
		if pos > 0 {
			reviewPayload["new_position"] = pos
		}
		if origPos > 0 {
			reviewPayload["old_position"] = origPos
		}
		reviewPayloadBytes, _ := json.Marshal(reviewPayload)
		output, err := forgejoPost(fmt.Sprintf("repos/%s/%s/pulls/%d/reviews/%s/comments", owner, repo, prNumber, reviewID), reviewPayloadBytes)
		if err == nil {
			return parseForgejoReplyResponse(output)
		}
	}

	// 3. Issue comment fallback (for summary comments or when strategy 2 failed)
	output, err = forgejoPost(fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, repo, prNumber), payload)
	if err != nil {
		return ReplyResult{Success: false, Error: err.Error()}, nil
	}
	var resp struct {
		HTMLURL string `json:"html_url"`
	}
	if json.Unmarshal([]byte(output), &resp) == nil && resp.HTMLURL != "" {
		return ReplyResult{Success: true, ReplyURL: resp.HTMLURL}, nil
	}
	return ReplyResult{Success: true}, nil
}

// parseForgejoReplyResponse extracts a ReplyResult from a successful Forgejo response.
func parseForgejoReplyResponse(output string) (ReplyResult, error) {
	var resp struct {
		HTMLURL string `json:"html_url"`
		URL     string `json:"url"`
	}
	if json.Unmarshal([]byte(output), &resp) == nil {
		if resp.HTMLURL != "" {
			return ReplyResult{Success: true, ReplyURL: resp.HTMLURL}, nil
		}
		if resp.URL != "" {
			return ReplyResult{Success: true, ReplyURL: resp.URL}, nil
		}
	}
	return ReplyResult{Success: true}, nil
}

// PostForgejoReply posts a reply to a Forgejo review comment via the REST API.
// When dryRun is true no request is made.
func PostForgejoReply(owner, repo string, prNumber int, commentID, body string, dryRun bool) (ReplyResult, error) {
	if dryRun {
		return ReplyResult{Success: true}, nil
	}

	reviewID, parent, err := findForgejoReviewIDForComment(owner, repo, prNumber, commentID)
	if err != nil {
		// Still try without review ID — reply endpoint doesn't need it
		return forgejoTryReply(owner, repo, prNumber, commentID, body, "", forgejoComment{})
	}

	return forgejoTryReply(owner, repo, prNumber, commentID, body, reviewID, parent)
}

// DetectForgejoBotName inspects the PR's review authors for a Mira bot and
// returns the first match, falling back to "miracodeai-bot".
func DetectForgejoBotName(owner, repo string, prNumber int) (string, error) {
	reviewsRaw, err := forgejoGet(fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", owner, repo, prNumber))
	if err != nil {
		return "miracodeai-bot", nil
	}
	var reviews []forgejoReview
	if err := json.Unmarshal([]byte(reviewsRaw), &reviews); err != nil {
		return "miracodeai-bot", nil
	}
	for _, review := range reviews {
		if IsMiraComment(review.User.Login) {
			return review.User.Login, nil
		}
	}
	return "miracodeai-bot", nil
}

// ResolveForgejoThread always fails: this Forgejo instance (Forgejo 16.0.3 /
// gitea-1.22.0 API) exposes no conversation-resolve endpoint — resolution is
// Enterprise-only. The failure is explicit so callers never mistake an
// unresolved thread for a resolved one.
func ResolveForgejoThread(owner, repo string, prNumber int, commentID string) ReplyResult {
	return ReplyResult{
		CommentID: commentID,
		Action:    "resolve",
		Success:   false,
		Error:     "resolving review conversations is not supported by this Forgejo instance (no resolve API endpoint)",
	}
}

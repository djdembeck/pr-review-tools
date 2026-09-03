package review

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// two-thread GraphQL fixture: thread T1 is resolved+outdated with a single
// root comment; thread T2 is open with a root comment and one reply.
const twoThreadGraphQLFixture = `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "nodes": [
            {
              "id": "PRRT_abc",
              "isResolved": true,
              "isOutdated": true,
              "comments": {
                "nodes": [
                  {
                    "databaseId": 101,
                    "body": "root in resolved thread",
                    "author": {"login": "miracodeai-bot"},
                    "createdAt": "2026-01-01T10:00:00Z"
                  }
                ]
              }
            },
            {
              "id": "PRRT_def",
              "isResolved": false,
              "isOutdated": false,
              "comments": {
                "nodes": [
                  {
                    "databaseId": 201,
                    "body": "open root",
                    "author": {"login": "nimuebot"},
                    "createdAt": "2026-01-02T10:00:00Z"
                  },
                  {
                    "databaseId": 202,
                    "body": "a reply",
                    "author": {"login": "humana"},
                    "createdAt": "2026-01-02T11:00:00Z",
                    "replyTo": {"databaseId": "201"}
                  }
                ]
              }
            }
          ]
        }
      }
    }
  }
}`

func parseFixture(t *testing.T) []RawComment {
	t.Helper()
	got, err := parseGitHubGraphQLResponse(twoThreadGraphQLFixture)
	if err != nil {
		t.Fatalf("parseGitHubGraphQLResponse: %v", err)
	}
	return got
}

// TestParseGitHubGraphQLResponseThreadID verifies every emitted comment
// (root AND reply) carries its thread's GraphQL node ID.
func TestParseGitHubGraphQLResponseThreadID(t *testing.T) {
	got := parseFixture(t)
	if len(got) != 3 {
		t.Fatalf("expected 3 comments, got %d", len(got))
	}
	byID := map[string]RawComment{}
	for _, c := range got {
		byID[c.ID] = c
	}
	if byID["101"].ThreadID == nil || *byID["101"].ThreadID != "PRRT_abc" {
		t.Errorf("comment 101: want ThreadID PRRT_abc, got %v", byID["101"].ThreadID)
	}
	if byID["201"].ThreadID == nil || *byID["201"].ThreadID != "PRRT_def" {
		t.Errorf("comment 201: want ThreadID PRRT_def, got %v", byID["201"].ThreadID)
	}
	if byID["202"].ThreadID == nil || *byID["202"].ThreadID != "PRRT_def" {
		t.Errorf("reply 202 must also carry ThreadID, got %v", byID["202"].ThreadID)
	}
}

// TestParseGitHubGraphQLResponseThreadFlags verifies per-thread flag
// propagation and reply shape.
func TestParseGitHubGraphQLResponseThreadFlags(t *testing.T) {
	got := parseFixture(t)
	byID := map[string]RawComment{}
	for _, c := range got {
		byID[c.ID] = c
	}
	if !byID["101"].IsResolved || !byID["101"].IsOutdated {
		t.Error("thread PRRT_abc flags must propagate to its comments")
	}
	if byID["201"].IsResolved || byID["201"].IsOutdated {
		t.Error("thread PRRT_def is open and current")
	}
	if byID["201"].ReplyToID != nil || byID["201"].ThreadReplies != 1 {
		t.Errorf("comment 201: want root with 1 reply, got ReplyToID=%v replies=%d", byID["201"].ReplyToID, byID["201"].ThreadReplies)
	}
	if byID["202"].ReplyToID == nil || *byID["202"].ReplyToID != "201" || byID["202"].ThreadReplies != 0 {
		t.Errorf("reply 202: want ReplyToID=201 and 0 replies, got %v/%d", byID["202"].ReplyToID, byID["202"].ThreadReplies)
	}

	parsed := ParseComment(byID["201"])
	if parsed.IsOutdated {
		t.Error("ParsedComment.IsOutdated must propagate")
	}
	if parsed.ThreadID == nil || *parsed.ThreadID != "PRRT_def" {
		t.Errorf("ParsedComment.ThreadID must propagate, got %v", parsed.ThreadID)
	}
}

// TestFindThreadID verifies lookup by root and reply IDs, plus the miss case.
func TestFindThreadID(t *testing.T) {
	got := parseFixture(t)
	tid, ok := FindThreadID(got, "202")
	if !ok || tid != "PRRT_def" {
		t.Fatalf("lookup by reply id: want PRRT_def ok, got %q %v", tid, ok)
	}
	tid, ok = FindThreadID(got, "101")
	if !ok || tid != "PRRT_abc" {
		t.Fatalf("lookup by root id: want PRRT_abc ok, got %q %v", tid, ok)
	}
	if _, ok := FindThreadID(got, "999"); ok {
		t.Fatal("missing id must return ok=false")
	}
}

// setGhRunEnv overrides the gh CLI seam for the duration of the test.
func setGhRunEnv(t *testing.T, fn func(args []string) (string, error)) {
	t.Helper()
	ghRunMu.Lock()
	origFn := ghRun
	ghRun = fn
	ghRunMu.Unlock()
	t.Cleanup(func() {
		ghRunMu.Lock()
		ghRun = origFn
		ghRunMu.Unlock()
	})
}

// ghField extracts a -F/-f key=value argument from a captured gh invocation.
func ghField(args []string, key string) (string, bool) {
	prefix := key + "="
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return strings.TrimPrefix(a, prefix), true
		}
	}
	return "", false
}

// ghCommentJSON renders one comment node with the required databaseId.
func ghCommentJSON(id int, extra string) string {
	return fmt.Sprintf(`{"databaseId":%d,"body":"comment %d","author":{"login":"user"},"createdAt":"2026-01-01T00:00:00Z"%s}`, id, id, extra)
}

// ghThreadJSON renders one thread node with the given comments and pageInfo.
// A nil comments slice renders an empty nodes array.
func ghThreadJSON(threadID string, comments []string, pageInfo string) string {
	return fmt.Sprintf(`{"id":%q,"isResolved":false,"isOutdated":false,"comments":{"nodes":[%s],"pageInfo":%s}}`, threadID, strings.Join(comments, ","), pageInfo)
}

// replyToOf formats a comment's ReplyToID for test error messages.
func replyToOf(c *RawComment) string {
	if c == nil || c.ReplyToID == nil {
		return "<none>"
	}
	return *c.ReplyToID
}

// TestFetchGitHubCommentsPagination drives FetchGitHubComments against a fake
// gh runner that returns 120 threads (two 100-per-page pages) and one thread
// with 60 comments (two 50-per-page pages), and verifies every page is
// fetched and FindThreadID resolves IDs from the second thread page and beyond
// the 50th comment in a thread.
func TestFetchGitHubCommentsPagination(t *testing.T) {
	const (
		bigThread   = "PRRT_big"
		bigRoot60   = "70060" // 60th comment in bigThread (beyond the first 50)
		page2Thread = "PRRT_119"
		page2Root   = "100119"
		page2Reply  = "200120"
	)
	seenThreads := 0
	seenComments := map[string]int{}
	pageInfoStop := `{"hasNextPage":false,"endCursor":null}`

	setGhRunEnv(t, func(args []string) (string, error) {
		query, _ := ghField(args, "query")
		var nodes []string
		var pageInfo string
		switch {
		case strings.Contains(query, "reviewThreads("):
			seenThreads++
			after, _ := ghField(args, "after")
			switch after {
			case "":
				// Page 1: PRRT_1..PRRT_99 (one comment each) + PRRT_big
				// (first 50 of 60 comments, hasNextPage).
				for id := 1; id <= 99; id++ {
					nodes = append(nodes, ghThreadJSON(fmt.Sprintf("PRRT_%d", id),
						[]string{ghCommentJSON(100000+id, "")}, pageInfoStop))
				}
				bigPage1 := make([]string, 0, 50)
				for id := 1; id <= 50; id++ {
					bigPage1 = append(bigPage1, ghCommentJSON(70000+id, ""))
				}
				nodes = append(nodes, ghThreadJSON(bigThread, bigPage1,
					`{"hasNextPage":true,"endCursor":"cursor-50"}`))
				pageInfo = `{"hasNextPage":true,"endCursor":"cursor-100"}`
			case "cursor-100":
				// Page 2: PRRT_101..PRRT_120, one comment each; PRRT_119
				// also has a reply.
				for id := 101; id <= 120; id++ {
					c := []string{ghCommentJSON(100000+id, "")}
					if id == 119 {
						c = append(c, ghCommentJSON(200120, `,"replyTo":{"databaseId":100119}`))
					}
					nodes = append(nodes, ghThreadJSON(fmt.Sprintf("PRRT_%d", id), c, pageInfoStop))
				}
				pageInfo = pageInfoStop
			default:
				return "", fmt.Errorf("unexpected threads cursor %q", after)
			}
			return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[%s],"pageInfo":%s}}}}}`,
				strings.Join(nodes, ","), pageInfo), nil
		case strings.Contains(query, "node(id:"):
			threadID, _ := ghField(args, "threadId")
			after, _ := ghField(args, "after")
			seenComments[threadID]++
			if threadID != bigThread {
				return "", fmt.Errorf("unexpected thread %q", threadID)
			}
			if after != "cursor-50" {
				return "", fmt.Errorf("thread comments query: want after=cursor-50, got %q", after)
			}
			// Page 2: comments 51..60. The response uses the Node interface
			// envelope (node { ... on PullRequestReviewThread { ... } }).
			for id := 51; id <= 60; id++ {
				nodes = append(nodes, ghCommentJSON(70000+id, ""))
			}
			out := fmt.Sprintf(`{"data":{"node":{"comments":{"nodes":[%s],"pageInfo":%s}}}}`,
				strings.Join(nodes, ","), pageInfoStop)
			return out, nil
		default:
			return "", fmt.Errorf("unexpected query: %.40s", query)
		}
	})

	got, err := FetchGitHubComments("owner", "repo", 42)
	if err != nil {
		t.Fatalf("FetchGitHubComments: %v", err)
	}

	// (1) All pages were fetched: two thread pages plus one comment page for
	// the 60-comment thread.
	if seenThreads != 2 {
		t.Fatalf("expected 2 thread page fetches, saw %d", seenThreads)
	}
	if n := seenComments[bigThread]; n != 1 {
		t.Fatalf("expected 1 additional comment page fetch for %s, got %d", bigThread, n)
	}

	// Every thread and every comment was accumulated: 99 + 20 single-comment
	// threads (PRRT_119 has a reply: +1) + 60 big-thread comments.
	if len(got) != 99+20+1+60 {
		t.Fatalf("expected 180 comments, got %d", len(got))
	}

	// (2) FindThreadID resolves a comment in the second thread page.
	if tid, ok := FindThreadID(got, page2Root); !ok || tid != page2Thread {
		t.Errorf("FindThreadID(%s): want %s, got %q ok=%v", page2Root, page2Thread, tid, ok)
	}
	// ...and a reply in the second thread page.
	if tid, ok := FindThreadID(got, page2Reply); !ok || tid != page2Thread {
		t.Errorf("FindThreadID(%s): want %s, got %q ok=%v", page2Reply, page2Thread, tid, ok)
	}
	// (3) FindThreadID resolves a comment beyond the 50th in a thread.
	if tid, ok := FindThreadID(got, bigRoot60); !ok || tid != bigThread {
		t.Errorf("FindThreadID(%s): want %s, got %q ok=%v", bigRoot60, bigThread, tid, ok)
	}
	// A comment in the first thread page still resolves.
	if tid, ok := FindThreadID(got, "100001"); !ok || tid != "PRRT_1" {
		t.Errorf("FindThreadID(100001): want PRRT_1, got %q ok=%v", tid, ok)
	}

	// The big thread's root comment reports all 59 replies once every comment
	// page is accumulated.
	var bigRoot *RawComment
	for i := range got {
		if got[i].ID == "70001" {
			bigRoot = &got[i]
		}
	}
	if bigRoot == nil {
		t.Fatalf("comment 70001 missing from result")
	}
	if bigRoot.ThreadReplies != 59 {
		t.Errorf("big thread root: want 59 replies after full pagination, got %d", bigRoot.ThreadReplies)
	}

	// The reply in the second thread page keeps its replyTo pointer.
	var reply *RawComment
	for i := range got {
		if got[i].ID == page2Reply {
			reply = &got[i]
		}
	}
	if reply == nil || reply.ReplyToID == nil || *reply.ReplyToID != page2Root {
		t.Errorf("reply %s: want ReplyToID %s, got %v", page2Reply, page2Root, replyToOf(reply))
	}
}

// TestCheckGitHubGraphQLPageMissingPullRequestData covers Gap 8 (P2):
// checkGitHubGraphQLPage must report "no pull request data in GraphQL
// response" when a reviewThreads page omits data.repository.pullRequest.
// The checker is package-visible, so it is called directly with a
// well-formed-shape page that omits pullRequest, and the error is also
// verified to surface from FetchGitHubComments.
func TestCheckGitHubGraphQLPageMissingPullRequestData(t *testing.T) {
	var resp graphQLResponse
	err := checkGitHubGraphQLPage(&resp, `{"data":{"repository":{}}}`)
	if err == nil || err.Error() != "no pull request data in GraphQL response" {
		t.Fatalf("checkGitHubGraphQLPage: want missing pullRequest error, got %v", err)
	}

	setGhRunEnv(t, func(args []string) (string, error) {
		query, _ := ghField(args, "query")
		if !strings.Contains(query, "reviewThreads(") {
			return "", fmt.Errorf("unexpected query: %.40s", query)
		}
		return `{"data":{"repository":{}}}`, nil
	})
	if _, err := FetchGitHubComments("owner", "repo", 42); err == nil ||
		err.Error() != "no pull request data in GraphQL response" {
		t.Fatalf("FetchGitHubComments: want missing pullRequest surface error, got %v", err)
	}
}

// TestFetchGitHubCommentsThreadCommentsPageNodeNull covers Gaps 6 and 9
// (both P2), which merge: a node(id:) page with data.node == null is
// rejected by the nil-node guard in checkGitHubThreadCommentsPage (github.go
// ~195-197, "no review thread data in GraphQL response") BEFORE the inline
// nil-node check in FetchGitHubComments (github.go ~147-149) — the checker
// returns first, so the inline check is shadowed and the guard's message is
// the error that surfaces. This single test drives the guard directly and
// its FetchGitHubComments surface, asserting the error and no panic.
func TestFetchGitHubCommentsThreadCommentsPageNodeNull(t *testing.T) {
	const threadID = "PRRT_nullnode"

	// Guard, driven directly: data.node == null is a missing node.
	var resp graphQLThreadCommentsResponse
	if err := checkGitHubThreadCommentsPage(&resp, `{"data":{"node":null}}`); err == nil ||
		err.Error() != "no review thread data in GraphQL response" {
		t.Fatalf("checkGitHubThreadCommentsPage: want nil-node error, got %v", err)
	}

	// Surface: a second comment page whose node(id:) response has
	// data.node == null must error (not panic) from FetchGitHubComments.
	setGhRunEnv(t, func(args []string) (string, error) {
		query, _ := ghField(args, "query")
		switch {
		case strings.Contains(query, "reviewThreads("):
			return fmt.Sprintf(
				`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[%s],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`,
				ghThreadJSON(threadID, []string{ghCommentJSON(1, "")}, `{"hasNextPage":true,"endCursor":"c2"}`)), nil
		case strings.Contains(query, "node(id:"):
			return `{"data":{"node":null}}`, nil
		}
		return "", fmt.Errorf("unexpected query: %.40s", query)
	})
	if _, err := FetchGitHubComments("owner", "repo", 42); err == nil ||
		err.Error() != "no review thread data in GraphQL response" {
		t.Fatalf("FetchGitHubComments: want nil-node surface error, got %v", err)
	}
}

// TestFetchGitHubCommentsThreadCommentsPageGraphQLError covers Gap 7 (P2):
// a node(id:) page carrying a top-level GraphQL errors array must surface
// checkGitHubThreadCommentsPage's "GraphQL error: <msg>" from
// FetchGitHubComments, even when the page's data.node is well formed
// (proving the errors branch, not the nil-node branch, fires).
func TestFetchGitHubCommentsThreadCommentsPageGraphQLError(t *testing.T) {
	const threadID = "PRRT_graphqlerr"
	setGhRunEnv(t, func(args []string) (string, error) {
		query, _ := ghField(args, "query")
		switch {
		case strings.Contains(query, "reviewThreads("):
			return fmt.Sprintf(
				`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[%s],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`,
				ghThreadJSON(threadID, []string{ghCommentJSON(1, "")}, `{"hasNextPage":true,"endCursor":"c2"}`)), nil
		case strings.Contains(query, "node(id:"):
			return `{"data":{"node":` + ghThreadJSON(threadID, nil, `{"hasNextPage":false,"endCursor":null}`) +
				`},"errors":[{"message":"thread comments query failed"}]}`, nil
		}
		return "", fmt.Errorf("unexpected query: %.40s", query)
	})
	if _, err := FetchGitHubComments("owner", "repo", 42); err == nil ||
		err.Error() != "GraphQL error: thread comments query failed" {
		t.Fatalf("FetchGitHubComments: want GraphQL error surface, got %v", err)
	}
}

// TestDetectGitHubBotNameGhRunErrorFallback covers Gap 10 (P2): when the gh
// CLI seam returns an error, DetectGitHubBotName must return the
// "miracodeai-bot" fallback with a nil error instead of propagating the
// failure.
func TestDetectGitHubBotNameGhRunErrorFallback(t *testing.T) {
	calls := 0
	setGhRunEnv(t, func(args []string) (string, error) {
		calls++
		return "", fmt.Errorf("gh command failed: network unreachable")
	})
	got, err := DetectGitHubBotName("owner", "repo", 42)
	if err != nil {
		t.Fatalf("DetectGitHubBotName: want nil error on gh failure, got %v", err)
	}
	if got != "miracodeai-bot" {
		t.Fatalf("want fallback miracodeai-bot, got %q", got)
	}
	if calls != 1 {
		t.Fatalf("expected 1 page fetch before fallback, got %d", calls)
	}
}

// TestFetchGitHubCommentsSinglePage verifies no behavior change for
// under-limit payloads: exactly one gh invocation, no "after" cursor sent.
func TestFetchGitHubCommentsSinglePage(t *testing.T) {
	// The two-thread fixture carries no pageInfo; a missing pageInfo
	// unmarshals to hasNextPage=false, so the paginated client must make
	// exactly one request with no after cursor.
	setGhRunEnv(t, func(args []string) (string, error) {
		query, _ := ghField(args, "query")
		if !strings.Contains(query, "reviewThreads(") {
			return "", fmt.Errorf("unexpected query: %.40s", query)
		}
		if _, ok := ghField(args, "after"); ok {
			return "", fmt.Errorf("under-limit fetch must not send an after cursor")
		}
		return twoThreadGraphQLFixture, nil
	})

	got, err := FetchGitHubComments("owner", "repo", 7)
	if err != nil {
		t.Fatalf("FetchGitHubComments: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 comments, got %d", len(got))
	}
	if tid, ok := FindThreadID(got, "201"); !ok || tid != "PRRT_def" {
		t.Errorf("FindThreadID(201): want PRRT_def, got %q ok=%v", tid, ok)
	}
}

// TestSelfLoginGitHubViaSeam verifies SelfLogin on GitHub routes through the
// guarded gh seam (not the real gh CLI) and returns the login from the seam.
func TestSelfLoginGitHubViaSeam(t *testing.T) {
	setGhRunEnv(t, func(args []string) (string, error) {
		if len(args) == 0 || args[0] != "api" {
			return "", fmt.Errorf("unexpected gh args: %v", args)
		}
		return "alice", nil
	})
	login, err := SelfLogin(PlatformGitHub)
	if err != nil {
		t.Fatalf("SelfLogin: %v", err)
	}
	if login != "alice" {
		t.Fatalf("want login alice, got %q", login)
	}
}

// botThreadJSON renders a review-thread node with a single comment authored
// by the given login, suitable for the DetectGitHubBotName query shape.
func botThreadJSON(login string) string {
	escaped, _ := json.Marshal(login)
	return fmt.Sprintf(
		`{"comments":{"nodes":[{"author":{"login":%s}}]}}`, escaped)
}

// TestDetectGitHubBotNamePagination verifies DetectGitHubBotName pages past
// the first 50 review threads and finds a Mira author on the second page, and
// that a first-page match returns without fetching a second page.
func TestDetectGitHubBotNamePagination(t *testing.T) {
	t.Run("second page match", func(t *testing.T) {
		calls := 0
		setGhRunEnv(t, func(args []string) (string, error) {
			calls++
			after, _ := ghField(args, "after")
			nodes := make([]string, 0, 50)
			var pageInfo string
			switch after {
			case "":
				// Page 1: 50 human-authored threads, more pages to come.
				for id := 1; id <= 50; id++ {
					nodes = append(nodes, botThreadJSON("humana"))
				}
				pageInfo = `{"hasNextPage":true,"endCursor":"bot-cursor-50"}`
			case "bot-cursor-50":
				// Page 2: a Mira-authored thread on this page. The "[bot]"
				// suffix must be stripped from the returned login.
				nodes = append(nodes, botThreadJSON("miracodeai[bot]"))
				pageInfo = `{"hasNextPage":false,"endCursor":null}`
			default:
				return "", fmt.Errorf("unexpected cursor %q", after)
			}
			return fmt.Sprintf(
				`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[%s],"pageInfo":%s}}}}}`,
				strings.Join(nodes, ","), pageInfo), nil

		})
		got, err := DetectGitHubBotName("owner", "repo", 42)
		if err != nil {
			t.Fatalf("DetectGitHubBotName: %v", err)
		}
		if got != "miracodeai" {
			t.Fatalf("want miracodeai, got %q", got)
		}
		if calls != 2 {
			t.Fatalf("expected 2 page fetches, got %d", calls)
		}
	})

	t.Run("first page match early-exits", func(t *testing.T) {
		calls := 0
		setGhRunEnv(t, func(args []string) (string, error) {
			calls++
			if _, ok := ghField(args, "after"); ok {
				return "", fmt.Errorf("first-page match must not send an after cursor")
			}
			return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[` +
				botThreadJSON("miracodeai[bot]") +
				`],"pageInfo":{"hasNextPage":true,"endCursor":"should-not-use"}}}}}}`, nil

		})
		got, err := DetectGitHubBotName("owner", "repo", 7)
		if err != nil {
			t.Fatalf("DetectGitHubBotName: %v", err)
		}
		if got != "miracodeai" {
			t.Fatalf("want miracodeai, got %q", got)
		}
		if calls != 1 {
			t.Fatalf("expected 1 page fetch on early exit, got %d", calls)
		}
	})
}

// TestDetectGitHubBotNameNoMatch verifies the fallback when no Mira author is
// found across all pages.
func TestDetectGitHubBotNameNoMatch(t *testing.T) {
	setGhRunEnv(t, func(args []string) (string, error) {
		if _, ok := ghField(args, "after"); ok {
			return "", fmt.Errorf("no second page expected")
		}
		return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[` +
			botThreadJSON("humana") +
			`],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`, nil
	})
	got, err := DetectGitHubBotName("owner", "repo", 9)
	if err != nil {
		t.Fatalf("DetectGitHubBotName: %v", err)
	}
	if got != "miracodeai-bot" {
		t.Fatalf("want fallback miracodeai-bot, got %q", got)
	}
}

// TestParseSinceArgRFC3339 verifies verbatim timestamp parsing.
func TestParseSinceArgRFC3339(t *testing.T) {
	ts, err := ParseSinceArg("2026-03-04T05:06:07Z")
	if err != nil {
		t.Fatalf("ParseSinceArg RFC3339: %v", err)
	}
	if ts.Unix() != 1772600767 {
		t.Fatalf("unexpected parsed time: %v", ts)
	}
}

// TestParseSinceArgInvalid verifies the error names the flag and value.
func TestParseSinceArgInvalid(t *testing.T) {
	_, err := ParseSinceArg("this-ref-does-not-exist-9f3b2a")
	if err == nil || !strings.Contains(err.Error(), `invalid --since value`) {
		t.Fatalf("want invalid --since error, got %v", err)
	}
}

// TestGitCommitTimeHead verifies HEAD resolves inside this repository.
func TestGitCommitTimeHead(t *testing.T) {
	ts, err := GitCommitTime("HEAD")
	if err != nil {
		t.Fatalf("GitCommitTime(HEAD): %v", err)
	}
	if ts.IsZero() {
		t.Fatal("expected non-zero HEAD commit time")
	}
}

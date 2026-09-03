package review

import (
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

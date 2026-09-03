package review

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFetchForgejoCommentsResolverMapping verifies that the resolver field
// (a user object, per the live Forgejo 16.0.3 swagger) maps to IsResolved:
// non-null resolver → resolved, null resolver → unresolved.
func TestFetchForgejoCommentsResolverMapping(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/own/rp/pulls/7/reviews":
			_ = json.NewEncoder(w).Encode([]forgejoReview{
				{ID: "11", User: forgejoUser{Login: "alice"}},
			})
		case "/api/v1/repos/own/rp/pulls/7/reviews/11/comments":
			_ = json.NewEncoder(w).Encode([]forgejoComment{
				{
					ID:       json.Number("101"),
					Body:     "resolved comment",
					User:     forgejoUser{Login: "alice"},
					Resolver: &forgejoUser{Login: "bob"},
				},
				{
					ID:       json.Number("102"),
					Body:     "open comment",
					User:     forgejoUser{Login: "alice"},
					Resolver: nil,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	setTestEnv(t, srv)

	got, err := FetchForgejoComments("own", "rp", 7)
	if err != nil {
		t.Fatalf("FetchForgejoComments: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(got))
	}
	byID := map[string]RawComment{}
	for _, c := range got {
		byID[c.ID] = c
	}
	if !byID["101"].IsResolved {
		t.Error("comment 101 with non-null resolver must be IsResolved")
	}
	if byID["102"].IsResolved {
		t.Error("comment 102 with null resolver must not be IsResolved")
	}
	if byID["101"].ThreadID != nil {
		t.Error("Forgejo comments must have nil ThreadID (non-threaded)")
	}
}

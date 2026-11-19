package review

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// withSelfLoginFunc stubs the exported self-login seam and restores it.
func withSelfLoginFunc(t *testing.T, fn func(context.Context, Platform) (string, error)) {
	t.Helper()
	prev := SelfLoginFunc
	SelfLoginFunc = fn
	t.Cleanup(func() { SelfLoginFunc = prev })
}

// TestResolveSelfLoginFailClosed verifies the fail-closed self-login contract:
// a SelfLogin failure or empty login MUST error (with a pointer at
// --include-self) instead of silently passing comments through unfiltered —
// on Forgejo, replies resurface as roots, so an unfiltered set would include
// the agent's own comments.
func TestResolveSelfLoginFailClosed(t *testing.T) {
	t.Run("login error", func(t *testing.T) {
		withSelfLoginFunc(t, func(context.Context, Platform) (string, error) {
			return "", errors.New("gh not authenticated")
		})
		if _, err := ResolveSelfLogin(context.Background(), PlatformGitHub); err == nil {
			t.Fatal("SelfLogin failure must error, not pass through unfiltered")
		} else if !strings.Contains(err.Error(), "--include-self") {
			t.Fatalf("error must direct the user to --include-self, got: %v", err)
		}
	})

	t.Run("empty login", func(t *testing.T) {
		withSelfLoginFunc(t, func(context.Context, Platform) (string, error) {
			return "", nil
		})
		if _, err := ResolveSelfLogin(context.Background(), PlatformForgejo); err == nil {
			t.Fatal("empty login must error, not pass through unfiltered")
		}
	})

	t.Run("valid login", func(t *testing.T) {
		withSelfLoginFunc(t, func(context.Context, Platform) (string, error) {
			return "Me", nil
		})
		login, err := ResolveSelfLogin(context.Background(), PlatformGitHub)
		if err != nil || login != "Me" {
			t.Fatalf("ResolveSelfLogin = %q, %v; want %q, nil", login, err, "Me")
		}
	})
}

// TestRelevantCommentsBaselineAndSelf verifies the shared relevance pipeline:
// baseline root IDs are captured BEFORE the resolved/outdated/self stages, a
// resolved thread stays invisible until reopened, and self-exclusion is
// case-insensitive.
func TestRelevantCommentsBaselineAndSelf(t *testing.T) {
	raw := []RawComment{
		{ID: "1", Author: "alice", IsResolved: true}, // resolved at start: in baseline, filtered out
		{ID: "2", Author: "bob"},                     // open root: in baseline and results
		{ID: "3", Author: "me"},                      // self-authored: in baseline, filtered out
	}

	var baseline []string
	res := RelevantComments(raw, RelevantOptions{SelfLogin: "Me"}, &baseline)

	// Baseline identities must be captured before the mutable stages.
	wantBaseline := []string{"1", "2", "3"}
	if len(baseline) != len(wantBaseline) {
		t.Fatalf("baseline = %v, want %v", baseline, wantBaseline)
	}
	for i := range wantBaseline {
		if baseline[i] != wantBaseline[i] {
			t.Fatalf("baseline = %v, want %v", baseline, wantBaseline)
		}
	}

	// Results: only the open, non-self root survives.
	if len(res.Comments) != 1 || res.Comments[0].ID != "2" {
		t.Fatalf("comments = %+v, want only root 2", res.Comments)
	}

	// Reopen the resolved thread: "1" and "2" are now relevant (self "3"
	// stays excluded), and neither is new relative to the baseline.
	raw[0].IsResolved = false
	res = RelevantComments(raw, RelevantOptions{SelfLogin: "Me"}, &baseline)
	if len(res.Comments) != 2 {
		t.Fatalf("after reopen comments = %+v, want roots 1 and 2", res.Comments)
	}
	for _, c := range res.Comments {
		if !containsString(baseline, c.ID) {
			t.Fatalf("reopened baseline root %s reported as new; baseline = %v", c.ID, baseline)
		}
	}
}

// TestRelevantCommentsFilters verifies the author and resolved/outdated
// stages of the shared pipeline.
func TestRelevantCommentsFilters(t *testing.T) {
	ptr := func(s string) *string { return &s }
	raw := []RawComment{
		{ID: "1", Author: "alice"},
		{ID: "2", Author: "bob", ReplyToID: ptr("2-root")}, // reply: never relevant
		{ID: "3", Author: "carol", IsOutdated: true},
	}

	res := RelevantComments(raw, RelevantOptions{Authors: []string{"alice", "carol"}}, nil)
	if len(res.Comments) != 1 || res.Comments[0].ID != "1" {
		t.Fatalf("author-filtered comments = %+v, want only root 1", res.Comments)
	}

	res = RelevantComments(raw, RelevantOptions{IncludeResolved: true, IncludeOutdated: true}, nil)
	if len(res.Comments) != 2 {
		t.Fatalf("include-all comments = %+v, want roots 1 and 3 (reply excluded)", res.Comments)
	}
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// TestParseCSVLogins verifies CSV splitting: trimming, empty-entry removal,
// and flag-shaped token detection. The os.Exit path for flag-shaped tokens is
// subprocess-tested by the binaries' flag-parse tests; here we cover the
// non-exit shapes.
func TestParseCSVLogins(t *testing.T) {
	got := ParseCSVLogins("trusted-authors", " nimuebot , copilot ,,")
	want := []string{"nimuebot", "copilot"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	if got := ParseCSVLogins("authors", "   , , "); len(got) != 0 {
		t.Fatalf("all-empty CSV must yield no logins, got %v", got)
	}

	cases := []struct {
		name string
		val  string
		want []string
	}{
		{"valid single login", "alice", []string{"alice"}},
		{"bare dash accepted", "-", []string{"-"}},
		{"empty string skipped", "", nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCSVLogins("authors", tt.val)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseCSVLogins(\"authors\", %q) = %v, want %v", tt.val, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("ParseCSVLogins(\"authors\", %q) = %v, want %v", tt.val, got, tt.want)
				}
			}
		})
	}
}

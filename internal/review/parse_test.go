package review

import (
	"testing"
	"time"
)

// pstr returns a pointer to s for constructing RawComment values inline.
func pstr(s string) *string { return &s }

// TestFilterRootCommentsAllAuthorsDefault locks the new default: with no
// author restriction, every root comment from ANY author (bots or humans) is
// kept and replies are never kept.
func TestFilterRootCommentsAllAuthorsDefault(t *testing.T) {
	comments := []RawComment{
		{ID: "1", Author: "miracodeai-bot", ReplyToID: nil},
		{ID: "2", Author: "nimuebot", ReplyToID: nil},
		{ID: "3", Author: "miracodeai", ReplyToID: pstr("1")},
		{ID: "4", Author: "human-reviewer", ReplyToID: nil},
	}
	got := FilterRootComments(comments)
	if len(got) != 3 {
		t.Fatalf("expected 3 root comments, got %d", len(got))
	}
	byID := map[string]RawComment{}
	for _, c := range got {
		byID[c.ID] = c
	}
	for _, want := range []string{"1", "2", "4"} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("expected root comment %s kept", want)
		}
	}
	if _, ok := byID["3"]; ok {
		t.Fatal("reply comment 3 must never be kept")
	}
}

// TestFilterRootCommentsWithAuthors verifies the author whitelist restricts
// output to matching roots only, regardless of Mira authorship.
func TestFilterRootCommentsWithAuthors(t *testing.T) {
	comments := []RawComment{
		{ID: "1", Author: "miracodeai-bot", ReplyToID: nil},
		{ID: "2", Author: "nimuebot", ReplyToID: nil},
		{ID: "3", Author: "nimuebot", ReplyToID: pstr("2")},
		{ID: "4", Author: "human", ReplyToID: nil},
	}
	got := FilterRootComments(comments, "nimuebot")
	if len(got) != 1 {
		t.Fatalf("expected 1 root comment, got %d", len(got))
	}
	if got[0].ID != "2" {
		t.Fatalf("expected nimuebot root comment 2, got %s", got[0].ID)
	}
}

// TestFilterRootCommentsExcludesReplies verifies replies are never kept
// regardless of author, whitelisted or otherwise.
func TestFilterRootCommentsExcludesReplies(t *testing.T) {
	comments := []RawComment{
		{ID: "1", Author: "miracodeai-bot", ReplyToID: pstr("0")},
		{ID: "2", Author: "nimuebot", ReplyToID: pstr("0")},
	}
	got := FilterRootComments(comments, "nimuebot")
	if len(got) != 0 {
		t.Fatalf("expected 0 comments, got %d", len(got))
	}
}

// TestFilterRootCommentsMatchingCases verifies matching is case-insensitive,
// trims spaces around CSV entries, and non-matching authors change nothing.
func TestFilterRootCommentsMatchingCases(t *testing.T) {
	comments := []RawComment{
		{ID: "1", Author: "miracodeai-bot", ReplyToID: nil},
		{ID: "2", Author: "NimueBot", ReplyToID: nil},
		{ID: "3", Author: "otherreviewer", ReplyToID: nil},
	}

	// main.go splits --authors CSV into individual logins before calling the
	// filter, so this test passes already-split entries that are
	// case-mismatched, whitespace-padded, or non-matching.
	got := FilterRootComments(comments, "nimbot", " NimueBot ", "other-reviewer")
	wantIDs := map[string]bool{"2": true}
	if len(got) != len(wantIDs) {
		t.Fatalf("expected %d comments, got %d", len(wantIDs), len(got))
	}
	for _, c := range got {
		if !wantIDs[c.ID] {
			t.Fatalf("unexpected comment %s kept", c.ID)
		}
	}
}

// TestFilterOutAuthors verifies self-exclusion: matching authors (case-
// insensitive) are dropped, everything else passes through, and an empty
// list is the identity function.
func TestFilterOutAuthors(t *testing.T) {
	comments := []RawComment{
		{ID: "1", Author: "me-the-agent"},
		{ID: "2", Author: "Me-The-Agent"},
		{ID: "3", Author: "nimuebot"},
	}
	got := FilterOutAuthors(comments, "me-the-agent")
	if len(got) != 1 || got[0].ID != "3" {
		t.Fatalf("expected only comment 3 to survive, got %+v", got)
	}

	if FilterOutAuthors(comments)[0].ID != "1" {
		t.Fatal("empty authors list must be identity")
	}
	if len(FilterOutAuthors(nil, "x")) != 0 {
		t.Fatal("nil input must stay empty")
	}
}

// TestFilterSince verifies strictly-after semantics and fail-open behavior
// for missing/unparseable timestamps.
func TestFilterSince(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	comments := []RawComment{
		{ID: "at-boundary", CreatedAt: pstr("2026-01-01T12:00:00Z")},
		{ID: "after", CreatedAt: pstr("2026-01-01T12:00:01Z")},
		{ID: "before", CreatedAt: pstr("2026-01-01T11:59:59Z")},
		{ID: "nil-ts"},
		{ID: "garbage-ts", CreatedAt: pstr("not-a-timestamp")},
	}

	got := FilterSince(comments, base)
	byID := map[string]bool{}
	for _, c := range got {
		byID[c.ID] = true
	}
	if byID["at-boundary"] {
		t.Fatal("comment exactly at boundary must be dropped (strictly after)")
	}
	if !byID["after"] {
		t.Fatal("comment after boundary must be kept")
	}
	if byID["before"] {
		t.Fatal("comment before boundary must be dropped")
	}
	if !byID["nil-ts"] {
		t.Fatal("missing timestamp must fail open (kept)")
	}
	if !byID["garbage-ts"] {
		t.Fatal("unparseable timestamp must fail open (kept)")
	}
}

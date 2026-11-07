package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djdembeck/pr-review-tools/internal/review"
)

// TestFilterOutSelf verifies the self-exclusion contract: self-authored
// comments are dropped case-insensitively and everything else is kept
// unchanged.
func TestFilterOutSelf(t *testing.T) {
	comments := []review.ParsedComment{
		{ID: "1", Author: "me"},
		{ID: "2", Author: "ME"},
		{ID: "3", Author: "nimuebot"},
	}
	got := filterOutSelf(comments, "Me")
	if len(got) != 1 || got[0].ID != "3" {
		t.Fatalf("expected only the non-self comment to survive, got %+v", got)
	}
}

// TestWatchFindsNewComment verifies the success path: a comment whose ID
// was not in the baseline is returned in fetch order.
func TestWatchFindsNewComment(t *testing.T) {
	calls := 0
	fetch := func() ([]review.ParsedComment, error) {
		calls++
		if calls == 1 {
			return []review.ParsedComment{{ID: "a"}}, nil
		}
		return []review.ParsedComment{{ID: "a"}, {ID: "b"}}, nil
	}
	baseline := []review.ParsedComment{{ID: "a"}}
	fresh, polls, waited, err := watch(baseline, 2*time.Second, 10*time.Millisecond, fetch)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if len(fresh) != 1 || fresh[0].ID != "b" {
		t.Fatalf("expected exactly the new comment b, got %+v", fresh)
	}
	if polls < 1 {
		t.Fatalf("expected at least one poll, got %d", polls)
	}
	if waited >= 2*time.Second {
		t.Fatalf("expected to finish before the timeout, waited %s", waited)
	}
}

// TestWatchTimesOut verifies the timeout path: no new comment before the
// deadline yields a timeout error and no comments.
func TestWatchTimesOut(t *testing.T) {
	fetch := func() ([]review.ParsedComment, error) {
		return []review.ParsedComment{{ID: "a"}}, nil
	}
	baseline := []review.ParsedComment{{ID: "a"}}
	fresh, _, _, err := watch(baseline, 100*time.Millisecond, 10*time.Millisecond, fetch)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "no new review comments") {
		t.Fatalf("timeout error must mention no new review comments, got: %v", err)
	}
	if fresh != nil {
		t.Fatalf("expected nil comments on timeout, got %+v", fresh)
	}
}

// TestWatchIgnoresTransientErrors verifies that transient fetch failures
// during polling are tolerated: the watch keeps going and still succeeds
// once a new comment appears.
func TestWatchIgnoresTransientErrors(t *testing.T) {
	calls := 0
	fetch := func() ([]review.ParsedComment, error) {
		calls++
		switch calls {
		case 1:
			return []review.ParsedComment{{ID: "a"}}, nil
		case 2, 3:
			return nil, errors.New("network blip")
		default:
			return []review.ParsedComment{{ID: "a"}, {ID: "b"}}, nil
		}
	}
	baseline := []review.ParsedComment{{ID: "a"}}
	fresh, _, _, err := watch(baseline, 2*time.Second, 10*time.Millisecond, fetch)
	if err != nil {
		t.Fatalf("expected success after transient errors, got: %v", err)
	}
	if len(fresh) != 1 || fresh[0].ID != "b" {
		t.Fatalf("expected exactly the new comment b, got %+v", fresh)
	}
}

// TestWatchBaselineIsFatal proves the baseline-diff semantics with a zero
// baseline: the first poll's comments are all "new" and immediately trigger
// success. (The baseline fetch itself lives in main()'s closure contract,
// which is out of reach in-process.)
func TestWatchBaselineIsFatal(t *testing.T) {
	fetch := func() ([]review.ParsedComment, error) {
		return []review.ParsedComment{{ID: "a"}}, nil
	}
	fresh, _, _, err := watch(nil, 2*time.Second, 10*time.Millisecond, fetch)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if len(fresh) != 1 || fresh[0].ID != "a" {
		t.Fatalf("expected the zero-baseline comment to be reported new, got %+v", fresh)
	}
}

// TestSelectRelevant verifies the relevant-comment contract: root-only,
// author filter, open, not outdated, self-excluded — in that order.
func TestSelectRelevant(t *testing.T) {
	raw := []review.RawComment{
		{ID: "alice-root", Author: "alice"},
		{ID: "bob-reply", Author: "bob", ReplyToID: strPtr("alice-root")},
		{ID: "me-root", Author: "me"},
		{ID: "carol-root", Author: "carol", IsResolved: true},
		{ID: "dave-root", Author: "dave", IsOutdated: true},
	}

	// No author filter: alice is the only comment that survives root-only,
	// resolved, outdated, and self filters.
	got := selectRelevant(raw, nil, nil, "me")
	if len(got) != 1 || got[0].ID != "alice-root" {
		t.Fatalf("expected only alice's root comment, got %+v", got)
	}

	// Author filter to carol: her root is the only author match, but the
	// resolved filter drops it, so the result is empty.
	got = selectRelevant(raw, []string{"CAROL"}, nil, "me")
	if len(got) != 0 {
		t.Fatalf("expected no comments for resolved carol root, got %+v", got)
	}
}

func strPtr(s string) *string { return &s }

// TestParseHead verifies the positional PR-number / help seam: --help and -h
// MUST short-circuit to help regardless of position or PR-number validity,
// while non-help invocations with an invalid PR number still error.
func TestParseHead(t *testing.T) {
	t.Run("help first arg, no PR number", func(t *testing.T) {
		for _, flag := range []string{"--help", "-h"} {
			pr, showHelp, err := parseHead([]string{flag})
			if !showHelp || pr != 0 || err != "" {
				t.Fatalf("parseHead([%s]) = %d, %v, %q; want help requested", flag, pr, showHelp, err)
			}
		}
	})

	t.Run("help with valid or invalid PR number", func(t *testing.T) {
		cases := [][]string{{"42", "--help"}, {"--help", "42"}, {"abc", "-h"}, {"-h", "--timeout", "5s"}}
		for _, args := range cases {
			if pr, showHelp, err := parseHead(args); !showHelp || pr != 0 || err != "" {
				t.Fatalf("parseHead(%v) = %d, %v, %q; want help requested", args, pr, showHelp, err)
			}
		}
	})

	t.Run("valid PR number", func(t *testing.T) {
		pr, showHelp, err := parseHead([]string{"42", "--timeout", "5s"})
		if showHelp || err != "" || pr != 42 {
			t.Fatalf("parseHead([42 --timeout 5s]) = %d, %v, %q; want prNumber 42, no help, no error", pr, showHelp, err)
		}
	})

	t.Run("invalid PR number still errors", func(t *testing.T) {
		cases := []struct {
			args []string
			want string
		}{
			{[]string{"abc"}, "Invalid PR number: abc"},
			{[]string{"0"}, "Invalid PR number: 0"},
			{[]string{"-3"}, "Invalid PR number: -3"},
		}
		for _, tt := range cases {
			pr, showHelp, err := parseHead(tt.args)
			if showHelp || pr != 0 {
				t.Fatalf("parseHead(%v) = %d, %v; want error, no help", tt.args, pr, showHelp)
			}
			if err != tt.want {
				t.Fatalf("parseHead(%v) error = %q, want %q", tt.args, err, tt.want)
			}
		}
	})
}

// TestUsageTextComplete verifies the banner printed for --help/-h and no args
// actually lists the documented flags, so the help cannot regress to a
// signpost-only message.
func TestUsageTextComplete(t *testing.T) {
	for _, want := range []string{
		"--timeout <duration>",
		"--poll <duration>",
		"--authors <csv>",
		"--trusted-authors <csv>",
		"--include-self",
		"--format json|consensus",
		"--help, -h",
	} {
		if !strings.Contains(usageText, want) {
			t.Errorf("usageText must document %q", want)
		}
	}
}

// TestParseCSVLogins verifies CSV splitting: trimming, empty-entry removal,
// and flag-shaped token rejection (caught at flag-parsing time, before any
// API call, so no os.Exit is reachable in-process for this shape).
func TestParseCSVLogins(t *testing.T) {
	got := parseCSVLogins("trusted-authors", " nimuebot , copilot ,,")
	want := []string{"nimuebot", "copilot"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	if got := parseCSVLogins("authors", "   , , "); len(got) != 0 {
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
			got := parseCSVLogins("authors", tt.val)
			if len(got) != len(tt.want) {
				t.Fatalf("parseCSVLogins(\"authors\", %q) = %v, want %v", tt.val, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("parseCSVLogins(\"authors\", %q) = %v, want %v", tt.val, got, tt.want)
				}
			}
		})
	}
}

// buildWatchBinary builds the watch binary once into a per-test temp dir and
// returns the binary path. Subprocess tests need it because the flag-parsing
// branches under test live inline in main() and os.Exit on the error path.
func buildWatchBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pr-review-watch")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building pr-review-watch: %v\n%s", err, out)
	}
	return bin
}

// runWatch runs the binary with the given args and reports exit status and
// combined stdout+stderr.
func runWatch(t *testing.T, bin string, args ...string) (int, string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running watch %v: %v", args, err)
		}
	}
	return code, string(out)
}

// TestFlagParseSubprocess drives the inline flag-parsing branches in main()
// through the real binary: invalid durations and flag-shaped author values
// are rejected before any API call, and a valid invocation clears
// flag-parsing (failing later, at the git/fetch stage, instead). Flag
// parsing runs before any git/API access, so none of these paths need a
// live remote.
func TestFlagParseSubprocess(t *testing.T) {
	bin := buildWatchBinary(t)

	cases := []struct {
		name          string
		args          []string
		wantCode      int
		wantInOutput  string // substring that MUST appear
		wantNotOutput string // substring that MUST NOT appear
	}{
		{
			name:         "timeout non-duration rejected",
			args:         []string{"42", "--timeout", "abc"},
			wantCode:     1,
			wantInOutput: "--timeout must be a positive duration",
		},
		{
			name:         "timeout zero rejected",
			args:         []string{"42", "--timeout", "0s"},
			wantCode:     1,
			wantInOutput: "--timeout must be a positive duration",
		},
		{
			name:         "poll negative rejected",
			args:         []string{"42", "--poll", "-5s"},
			wantCode:     1,
			wantInOutput: "--poll must be a positive duration",
		},
		{
			name:         "authors flag-shaped value rejected",
			args:         []string{"42", "--authors", "--include-self"},
			wantCode:     1,
			wantInOutput: "authors got flag-shaped value",
		},
		{
			name:         "authors empty CSV requires non-empty value",
			args:         []string{"42", "--authors", ""},
			wantCode:     1,
			wantInOutput: "Error: --authors requires a non-empty value",
		},
		{
			// Flag parsing succeeds; the run must fail (if at all) at the
			// git/fetch stage, never with a flag-parsing error.
			name:          "valid args pass flag parsing",
			args:          []string{"99999", "--timeout", "5s"},
			wantCode:      1,
			wantNotOutput: "flag-shaped",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			code, out := runWatch(t, bin, tt.args...)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d; output:\n%s", code, tt.wantCode, out)
			}
			if tt.wantInOutput != "" && !strings.Contains(out, tt.wantInOutput) {
				t.Fatalf("output must contain %q:\n%s", tt.wantInOutput, out)
			}
			if tt.wantNotOutput != "" && strings.Contains(out, tt.wantNotOutput) {
				t.Fatalf("output must not contain %q:\n%s", tt.wantNotOutput, out)
			}
		})
	}
}

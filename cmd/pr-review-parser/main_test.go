package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/djdembeck/pr-review-tools/internal/review"
)

// withSelfLogin stubs the self-login seam and returns a restore func.
func withSelfLogin(t *testing.T, fn func(review.Platform) (string, error)) {
	t.Helper()
	prev := selfLoginFunc
	selfLoginFunc = fn
	t.Cleanup(func() { selfLoginFunc = prev })
}

// TestExcludeSelfFailClosed verifies the self-exclusion contract: a
// SelfLogin failure or empty login MUST error instead of silently passing
// comments through unfiltered (replies resurface as roots on Forgejo), and
// the error must direct the user to --include-self.
func TestExcludeSelfFailClosed(t *testing.T) {
	comments := []review.ParsedComment{
		{ID: "1", Author: "me"},
		{ID: "2", Author: "nimuebot"},
	}

	t.Run("SelfLogin error", func(t *testing.T) {
		withSelfLogin(t, func(review.Platform) (string, error) {
			return "", errors.New("gh not authenticated")
		})
		got, err := excludeSelf(comments, review.PlatformGitHub)
		if err == nil {
			t.Fatal("SelfLogin failure must error, not silently pass comments through")
		}
		if got != nil {
			t.Fatalf("expected nil comments on error, got %+v", got)
		}
		if !strings.Contains(err.Error(), "--include-self") {
			t.Fatalf("error must direct the user to --include-self, got: %v", err)
		}
	})

	t.Run("empty login", func(t *testing.T) {
		withSelfLogin(t, func(review.Platform) (string, error) {
			return "", nil
		})
		if _, err := excludeSelf(comments, review.PlatformForgejo); err == nil {
			t.Fatal("empty login must error, not silently pass comments through")
		}
	})
}

// TestExcludeSelfFilters verifies the success path: self-authored comments
// are dropped case-insensitively and everything else is kept unchanged.
func TestExcludeSelfFilters(t *testing.T) {
	withSelfLogin(t, func(review.Platform) (string, error) {
		return "Me", nil
	})
	comments := []review.ParsedComment{
		{ID: "1", Author: "me"},
		{ID: "2", Author: "ME"},
		{ID: "3", Author: "nimuebot"},
	}
	got, err := excludeSelf(comments, review.PlatformGitHub)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "3" {
		t.Fatalf("expected only the non-self comment to survive, got %+v", got)
	}
}

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
		cases := [][]string{{"42", "--help"}, {"--help", "42"}, {"abc", "-h"}, {"-h", "--format", "json"}}
		for _, args := range cases {
			if pr, showHelp, err := parseHead(args); !showHelp || pr != 0 || err != "" {
				t.Fatalf("parseHead(%v) = %d, %v, %q; want help requested", args, pr, showHelp, err)
			}
		}
	})

	t.Run("valid PR number", func(t *testing.T) {
		pr, showHelp, err := parseHead([]string{"42", "--format", "json"})
		if showHelp || err != "" || pr != 42 {
			t.Fatalf("parseHead([42 --format json]) = %d, %v, %q; want prNumber 42, no help, no error", pr, showHelp, err)
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
		"--format json|consensus",
		"--include-resolved",
		"--authors <csv>",
		"--trusted-authors <csv>",
		"--include-self",
		"--include-outdated",
		"--since <ref|RFC3339>",
		"--since-last-commit",
		"--since-last-push",
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

// buildParserBinary builds the parser once into a per-test temp dir and
// returns the binary path. Subprocess tests need it because the flag-parsing
// branches under test live inline in main() and os.Exit on the error path.
func buildParserBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pr-review-parser")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building pr-review-parser: %v\n%s", err, out)
	}
	return bin
}

// runParser runs the binary with the given args and reports exit status and
// combined stdout+stderr.
func runParser(t *testing.T, bin string, args ...string) (int, string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running parser %v: %v", args, err)
		}
	}
	return code, string(out)
}

// TestFlagParseAuthorsSubprocess drives the inline flag-parsing branches in
// main() through the real binary: --authors/--trusted-authors with a
// flag-shaped value are rejected before any API call, empty-after-parse CSVs
// hit the "requires a non-empty value" branch, and a valid CSV clears
// flag-parsing (failing later, at the network boundary, on a non-existent PR
// instead). Flag parsing runs before any git/API access, so none of these
// paths need a live remote.
func TestFlagParseAuthorsSubprocess(t *testing.T) {
	bin := buildParserBinary(t)

	cases := []struct {
		name          string
		args          []string
		wantCode      int
		wantInOutput  string // substring that MUST appear
		wantNotOutput string // substring that MUST NOT appear
	}{
		{
			name:         "authors flag-shaped value rejected",
			args:         []string{"42", "--authors", "--include-self"},
			wantCode:     1,
			wantInOutput: "authors got flag-shaped value '--include-self', expected CSV of logins",
		},
		{
			name:         "authors empty CSV requires non-empty value",
			args:         []string{"42", "--authors", ""},
			wantCode:     1,
			wantInOutput: "Error: --authors requires a non-empty value",
		},
		{
			name:         "authors whitespace-only CSV requires non-empty value",
			args:         []string{"42", "--authors", "   "},
			wantCode:     1,
			wantInOutput: "Error: --authors requires a non-empty value",
		},
		{
			name:         "trusted-authors empty CSV requires non-empty value",
			args:         []string{"42", "--trusted-authors", ""},
			wantCode:     1,
			wantInOutput: "Error: --trusted-authors requires a non-empty value",
		},
		{
			name:         "trusted-authors whitespace-only CSV requires non-empty value",
			args:         []string{"42", "--trusted-authors", " , "},
			wantCode:     1,
			wantInOutput: "Error: --trusted-authors requires a non-empty value",
		},
		{
			// Flag parsing succeeds; the run must fail (if at all) at the
			// git/fetch stage, never with a flag-parsing error.
			name:          "authors valid CSV passes flag parsing",
			args:          []string{"99999", "--authors", "alice , bob"},
			wantCode:      1,
			wantNotOutput: "flag-shaped",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			code, out := runParser(t, bin, tt.args...)
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

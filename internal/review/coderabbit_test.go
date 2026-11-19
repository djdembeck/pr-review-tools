package review

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

const crFence = "```"

// crFixtureBody mirrors the real structure of CodeRabbit review 5967471268
// (laxamentumtech/curation-workbench PR 49), trimmed: the "Prompt to fix all
// issues" summary line, opening fence, intro paragraph, an Inline comments
// section with one file group, a --- separator, an Outside diff comments
// section with a single-line item, a range item, and a wrapped multi-line
// item, then ---, Minor comments, Failed pre-merge checks, closing fence.
func crFixtureBody() string {
	return "<details>\n" +
		"<summary>Prompt to fix all issues</summary>\n" +
		"\n" +
		crFence + "\n" +
		"CodeRabbit summarizes the changes in this pull request.\n" +
		"\n" +
		"Inline comments:\n" +
		"Review comments at @internal/modules/library/apply.go:\n" +
		"- Line 120: an inline finding that must not be emitted\n" +
		"\n" +
		"---\n" +
		"\n" +
		"Outside diff comments:\n" +
		"Review comments at @internal/modules/library/apply.go:\n" +
		"- Line 1154: The new function leaks the connection on early return.\n" +
		"- Around line 679-681: The retry loop caps attempts but the backoff is unbounded.\n" +
		"- Line 400: This helper duplicates the logic already in\n" +
		"  the validator package; extract it.\n" +
		"\n" +
		"Review comments at @scripts/library-agent.py:\n" +
		"- Line 188: Validate the endpoint is HTTPS before sending the token.\n" +
		"\n" +
		"---\n" +
		"\n" +
		"Minor comments:\n" +
		"Review comments at @internal/modules/library/apply.go:\n" +
		"- Line 10: a minor nit that must not be emitted\n" +
		"\n" +
		"Failed pre-merge checks:\n" +
		"- lint\n" +
		crFence + "\n" +
		"</details>"
}

// crMinimalBody renders the smallest body that carries one outside-diff
// finding for path/line/text.
func crMinimalBody(path string, line int, text string) string {
	return "Prompt to fix all issues\n" +
		crFence + "\n" +
		"Outside diff comments:\n" +
		"Review comments at @" + path + ":\n" +
		"- Line " + strconv.Itoa(line) + ": " + text + "\n" +
		crFence
}

// crReview builds a coderabbitReview; submittedAt "" yields a nil SubmittedAt.
func crReview(id, body, submittedAt, login string) coderabbitReview {
	var r coderabbitReview
	r.ID = json.Number(id)
	r.Body = body
	if submittedAt != "" {
		s := submittedAt
		r.SubmittedAt = &s
	}
	r.User.Login = login
	return r
}

// TestParseCoderabbitOutsideDiffFindings verifies the full fixture yields
// exactly the outside-diff findings with literal field values.
func TestParseCoderabbitOutsideDiffFindings(t *testing.T) {
	submitted := "2026-01-02T00:00:00Z"
	review := crReview("5967471268", crFixtureBody(), submitted, "coderabbitai[bot]")
	got := ParseCoderabbitOutsideDiffFindings(review)

	if len(got) != 4 {
		t.Fatalf("expected exactly 4 outside-diff findings, got %d:\n%+v", len(got), got)
	}

	// (a) single-line item
	c := got[0]
	if c.Path == nil || *c.Path != "internal/modules/library/apply.go" {
		t.Errorf("finding 0 path: want internal/modules/library/apply.go (no trailing colon), got %v", c.Path)
	}
	if c.StartLine == nil || *c.StartLine != 1154 {
		t.Errorf("finding 0 startLine: want 1154, got %v", c.StartLine)
	}
	if c.Line == nil || *c.Line != 1154 {
		t.Errorf("finding 0 line: want 1154, got %v", c.Line)
	}
	if c.Body != "The new function leaks the connection on early return." {
		t.Errorf("finding 0 body: %q", c.Body)
	}
	assertCRMeta(t, "finding 0", &c, "coderabbitai[bot]", submitted)

	// (b) range item
	c = got[1]
	if c.StartLine == nil || *c.StartLine != 679 {
		t.Errorf("finding 1 startLine: want 679, got %v", c.StartLine)
	}
	if c.Line == nil || *c.Line != 681 {
		t.Errorf("finding 1 line: want 681, got %v", c.Line)
	}
	if c.Body != "The retry loop caps attempts but the backoff is unbounded." {
		t.Errorf("finding 1 body: %q", c.Body)
	}
	assertCRMeta(t, "finding 1", &c, "coderabbitai[bot]", submitted)

	// (c) wrapped multi-line item joined with \n
	c = got[2]
	if c.Path == nil || *c.Path != "internal/modules/library/apply.go" {
		t.Errorf("finding 2 path: got %v", c.Path)
	}
	if c.StartLine == nil || *c.StartLine != 400 || c.Line == nil || *c.Line != 400 {
		t.Errorf("finding 2 lines: start=%v end=%v", c.StartLine, c.Line)
	}
	if want := "This helper duplicates the logic already in\n  the validator package; extract it."; c.Body != want {
		t.Errorf("finding 2 body:\ngot  %q\nwant %q", c.Body, want)
	}
	assertCRMeta(t, "finding 2", &c, "coderabbitai[bot]", submitted)

	// (d) second file group: the path must be the group's own, not the
	// first group's (guards against Path aliasing the shared group var).
	c = got[3]
	if c.Path == nil || *c.Path != "scripts/library-agent.py" {
		t.Errorf("finding 3 path: want scripts/library-agent.py, got %v", c.Path)
	}
	if c.StartLine == nil || *c.StartLine != 188 || c.Line == nil || *c.Line != 188 {
		t.Errorf("finding 3 lines: start=%v end=%v", c.StartLine, c.Line)
	}
	if c.Body != "Validate the endpoint is HTTPS before sending the token." {
		t.Errorf("finding 3 body: %q", c.Body)
	}
	assertCRMeta(t, "finding 3", &c, "coderabbitai[bot]", submitted)

	// The inline, minor, and failed-checks sections must NOT be emitted.
	for _, c := range got {
		if c.Body == "an inline finding that must not be emitted" ||
			c.Body == "a minor nit that must not be emitted" ||
			c.Body == "lint" {
			t.Errorf("section content leaked into findings: %q", c.Body)
		}
	}
}

// TestParseCoderabbitPromptWordings verifies each observed CodeRabbit
// <summary> phrasing gates the same parse.
func TestParseCoderabbitPromptWordings(t *testing.T) {
	for _, summary := range []string{
		"Prompt to fix all issues",
		"Prompt to fix review comments",
		"Prompt for all review comments with AI agents",
		"Prompt for AI Agents",
	} {
		t.Run(summary, func(t *testing.T) {
			body := "<summary>" + summary + "</summary>\n" +
				crFence + "\n" +
				"Outside diff comments:\n" +
				"Review comments at @f.go:\n" +
				"- Line 7: a finding\n" +
				crFence
			review := crReview("9", body, "2026-01-01T00:00:00Z", "coderabbitai[bot]")
			got := ParseCoderabbitOutsideDiffFindings(review)
			if len(got) != 1 || *got[0].Path != "f.go" || *got[0].StartLine != 7 {
				t.Fatalf("summary %q: want 1 finding at f.go:7, got %+v", summary, got)
			}
		})
	}
}

// TestParseCoderabbitInBacktickGroup verifies the "In `@path`:" group-prefix
// variant parses to the same path as the "Review comments at @" form.
func TestParseCoderabbitInBacktickGroup(t *testing.T) {
	body := "Prompt to fix all issues\n" +
		crFence + "\n" +
		"Outside diff comments:\n" +
		"In `@agents/code-reviewer.md`:\n" +
		"- Around line 61-74: trim the checklist\n" +
		crFence
	review := crReview("4921214429", body, "2026-01-01T00:00:00Z", "coderabbitai[bot]")
	got := ParseCoderabbitOutsideDiffFindings(review)
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %+v", got)
	}
	c := got[0]
	if c.Path == nil || *c.Path != "agents/code-reviewer.md" {
		t.Fatalf("path: want agents/code-reviewer.md (no leading @), got %v", c.Path)
	}
	if c.StartLine == nil || *c.StartLine != 61 || c.Line == nil || *c.Line != 74 {
		t.Fatalf("lines: start=%v end=%v", c.StartLine, c.Line)
	}
}

// TestParseCoderabbitEpilogue verifies the prompt-block epilogue is excluded
// from the last finding's body, both as a trailing paragraph and as the
// section terminator.
func TestParseCoderabbitEpilogue(t *testing.T) {
	body := "Prompt to fix all issues\n" +
		crFence + "\n" +
		"Outside diff comments:\n" +
		"Review comments at @src/app.ts:\n" +
		"- Line 8: update the example to parser-compatible decimal points.\n" +
		"\n" +
		"After applying the fix, consider running `coderabbit review --agent` for local\n" +
		"review. Visit https://docs.coderabbit.ai/cli?utm_source=ghpr\n" +
		crFence + "\n" +
		"</dd>"
	review := crReview("5729945191", body, "2026-01-01T00:00:00Z", "coderabbitai[bot]")
	got := ParseCoderabbitOutsideDiffFindings(review)
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %+v", got)
	}
	c := got[0]
	if c.Body != "update the example to parser-compatible decimal points." {
		t.Fatalf("body must not carry the epilogue:\n%q", c.Body)
	}
	if c.StartLine == nil || *c.StartLine != 8 || c.Line == nil || *c.Line != 8 {
		t.Fatalf("lines: start=%v end=%v", c.StartLine, c.Line)
	}
}

// assertCRMeta checks the platform-independent RawComment fields that must be
// set identically on every synthetic finding.
func assertCRMeta(t *testing.T, label string, c *RawComment, author, submittedAt string) {
	t.Helper()
	if !strings.HasPrefix(c.ID, "cr-") {
		t.Errorf("%s: ID %q must have the cr- prefix", label, c.ID)
	}
	if c.Author != author {
		t.Errorf("%s: author: want %q, got %q", label, author, c.Author)
	}
	if c.CreatedAt == nil || *c.CreatedAt != submittedAt {
		t.Errorf("%s: createdAt: want %q, got %v", label, submittedAt, c.CreatedAt)
	}
	if c.IsResolved {
		t.Errorf("%s: IsResolved must be false", label)
	}
	if c.IsOutdated {
		t.Errorf("%s: IsOutdated must be false", label)
	}
	if c.ReplyToID != nil {
		t.Errorf("%s: ReplyToID must be nil", label)
	}
	if c.ThreadID != nil {
		t.Errorf("%s: ThreadID must be nil", label)
	}
	if c.DiffHunk != nil {
		t.Errorf("%s: DiffHunk must be nil", label)
	}
}

// TestParseCoderabbitNegative verifies the fail-open paths: no summary line,
// no fence, no section, and an empty body all yield nil.
func TestParseCoderabbitNegative(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"no summary line", crFence + "\nOutside diff comments:\nReview comments at @f.go:\n- Line 1: x\n" + crFence},
		{"summary present, no fence", "Prompt to fix all issues\nno fence here"},
		{"summary and fence, no outside diff section", "Prompt to fix all issues\n" + crFence + "\nInline comments:\n- Line 1: x\n" + crFence},
		{"section outside the fence", "Prompt to fix all issues\n" + crFence + "\nclosed fence\n" + crFence + "\nOutside diff comments:\nReview comments at @f.go:\n- Line 1: x\n"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			review := crReview("1", tt.body, "2026-01-01T00:00:00Z", "coderabbitai[bot]")
			if got := ParseCoderabbitOutsideDiffFindings(review); got != nil {
				t.Fatalf("expected nil, got %d findings: %+v", len(got), got)
			}
		})
	}
}

// TestParseCoderabbitDedupe verifies two identical findings in the section
// emit once, and that items without a preceding file group are skipped.
func TestParseCoderabbitDedupe(t *testing.T) {
	body := "Prompt to fix all issues\n" +
		crFence + "\n" +
		"Outside diff comments:\n" +
		"- Line 5: orphan item with no file group, must be skipped\n" +
		"Review comments at @f.go:\n" +
		"- Line 1: same text\n" +
		"- Line 1: same text\n" +
		"- Line 2: same text\n" +
		crFence
	review := crReview("1", body, "2026-01-01T00:00:00Z", "coderabbitai[bot]")
	got := ParseCoderabbitOutsideDiffFindings(review)
	if len(got) != 2 {
		t.Fatalf("want 2 findings (orphan skipped, duplicate collapsed), got %d: %+v", len(got), got)
	}
	if *got[0].StartLine != 1 || *got[1].StartLine != 2 {
		t.Fatalf("finding order/lines: %v %v", *got[0].StartLine, *got[1].StartLine)
	}
}

// TestCollectCoderabbitFindings verifies newest-only selection by parsed
// instant (not RFC3339 string order), nil/unparseable timestamp exclusion,
// and first-in-list tie-breaking.
func TestCollectCoderabbitFindings(t *testing.T) {
	cr := "coderabbitai[bot]"

	t.Run("newer review without section is skipped, older findings kept", func(t *testing.T) {
		reviews := []coderabbitReview{
			crReview("1", crMinimalBody("a.go", 1, "older finding"), "2026-01-01T00:00:00Z", cr),
			crReview("2", "<details>no section here</details>", "2026-01-02T00:00:00Z", cr),
		}
		got := collectCoderabbitFindings(reviews)
		if len(got) != 1 || *got[0].Path != "a.go" {
			t.Fatalf("want the older review's finding (newer has no section), got %+v", got)
		}
	})

	t.Run("empty newest status review is skipped, older findings kept", func(t *testing.T) {
		// Mirrors the live case: CodeRabbit posts an empty-body APPROVED
		// review minutes after its substantive one.
		reviews := []coderabbitReview{
			crReview("1", crMinimalBody("a.go", 1, "real finding"), "2026-01-01T00:00:00Z", cr),
			crReview("2", "", "2026-01-01T00:04:00Z", cr),
		}
		got := collectCoderabbitFindings(reviews)
		if len(got) != 1 || *got[0].Path != "a.go" || got[0].Body != "real finding" {
			t.Fatalf("want the older review's finding past the empty newest review, got %+v", got)
		}
	})

	t.Run("newer review with its own findings wins over older", func(t *testing.T) {
		reviews := []coderabbitReview{
			crReview("1", crMinimalBody("a.go", 1, "older finding"), "2026-01-01T00:00:00Z", cr),
			crReview("2", crMinimalBody("b.go", 2, "newer finding"), "2026-01-02T00:00:00Z", cr),
		}
		got := collectCoderabbitFindings(reviews)
		if len(got) != 1 || *got[0].Path != "b.go" {
			t.Fatalf("want the newer review's finding, got %+v", got)
		}
	})

	t.Run("human login containing the substring is not a candidate", func(t *testing.T) {
		reviews := []coderabbitReview{
			crReview("1", crMinimalBody("a.go", 1, "sneaky finding"), "2026-01-01T00:00:00Z", "coderabbit-helper"),
			crReview("2", crMinimalBody("b.go", 2, "bot finding"), "2026-01-01T00:00:00Z", cr),
		}
		got := collectCoderabbitFindings(reviews)
		if len(got) != 1 || *got[0].Path != "b.go" {
			t.Fatalf("want only the exact bot login's finding, got %+v", got)
		}
	})

	t.Run("exact login match is case-insensitive", func(t *testing.T) {
		reviews := []coderabbitReview{
			crReview("1", crMinimalBody("a.go", 1, "case finding"), "2026-01-01T00:00:00Z", "CoderabbitAI[Bot]"),
		}
		if got := collectCoderabbitFindings(reviews); len(got) != 1 {
			t.Fatalf("want the case-variant bot login's finding, got %+v", got)
		}
	})

	t.Run("offsets compared as instants, not strings", func(t *testing.T) {
		// 2026-01-02T00:00:00+02:00 is 22:00Z — EARLIER than
		// 2026-01-01T23:00:00Z even though the string sorts after it.
		reviews := []coderabbitReview{
			crReview("1", crMinimalBody("plus.go", 1, "offset finding"), "2026-01-02T00:00:00+02:00", cr),
			crReview("2", crMinimalBody("zulu.go", 2, "zulu finding"), "2026-01-01T23:00:00Z", cr),
		}
		got := collectCoderabbitFindings(reviews)
		if len(got) != 1 || *got[0].Path != "zulu.go" {
			t.Fatalf("want the Zulu-offset review's finding, got %+v", got)
		}
	})

	t.Run("tie broken by first in list", func(t *testing.T) {
		ts := "2026-01-01T00:00:00Z"
		reviews := []coderabbitReview{
			crReview("1", crMinimalBody("first.go", 1, "first"), ts, cr),
			crReview("2", crMinimalBody("second.go", 2, "second"), ts, cr),
		}
		got := collectCoderabbitFindings(reviews)
		if len(got) != 1 || *got[0].Path != "first.go" {
			t.Fatalf("want the first-in-list review's finding, got %+v", got)
		}
	})

	t.Run("nil or unparseable submitted_at is not a candidate", func(t *testing.T) {
		reviews := []coderabbitReview{
			crReview("1", crMinimalBody("nada.go", 1, "no timestamp"), "", cr),
			crReview("2", crMinimalBody("bad.go", 2, "bad timestamp"), "not-a-time", cr),
			crReview("3", crMinimalBody("good.go", 3, "good timestamp"), "2026-01-01T00:00:00Z", cr),
		}
		got := collectCoderabbitFindings(reviews)
		if len(got) != 1 || *got[0].Path != "good.go" {
			t.Fatalf("want only the parseable review's finding, got %+v", got)
		}
	})

	t.Run("no candidates yields nil", func(t *testing.T) {
		reviews := []coderabbitReview{
			crReview("1", crMinimalBody("a.go", 1, "x"), "2026-01-01T00:00:00Z", "some-human"),
		}
		if got := collectCoderabbitFindings(reviews); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})
}

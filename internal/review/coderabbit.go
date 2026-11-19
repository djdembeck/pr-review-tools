package review

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// coderabbitBotLogin is the exact review-author login CodeRabbit posts under.
// Exact (case-insensitive) matching keeps a human login that merely contains
// the substring (e.g. coderabbit-helper) from entering newest-review
// selection, where it could suppress the genuine bot's findings.
const coderabbitBotLogin = "coderabbitai[bot]"

// coderabbitReview is the subset of a GitHub/Forgejo PR review this parser
// needs: the body (where CodeRabbit writes its agent-prompt block, whose
// <summary> line carries one of the crPromptSummaries phrasings), the
// submission timestamp, and the author login.
type coderabbitReview struct {
	ID          json.Number `json:"id"`
	Body        string      `json:"body"`
	SubmittedAt *string     `json:"submitted_at"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

// reCRSection locates the "Outside diff comments:" section inside the
// CodeRabbit "Prompt to fix all issues" block. The section runs to the next
// sibling section header ("Minor comments:" / "Failed pre-merge checks:"), a
// trailing "---" separator, or the end of the text.
// crEpilogueStart ends the prompt-block epilogue ("After applying the fix,
// consider running … Visit https://docs.coderabbit.ai/cli?…") when it appears
// at the end of the section, inside the fence. The section parser stops the
// last finding at this line.
const crEpilogueStart = "After applying the fix"

// reCRSection locates the "Outside diff comments:" section inside the
// CodeRabbit prompt block. The section runs to the next sibling section
// header ("Minor comments:" / "Failed pre-merge checks:"), a trailing "---"
// separator, the epilogue, or the end of the text.
var reCRSection = regexp.MustCompile(`(?m)^Outside diff comments:\r?\n([\s\S]*?)(?:\n(?:Minor comments:|Failed pre-merge checks:)|\n---\r?$|\n` + crEpilogueStart + `|\z)`)

// reCRGroupAt matches a "Review comments at @<path>:" file-group header.
var reCRGroupAt = regexp.MustCompile(`^Review comments at @(.+?):\s*$`)

// reCRGroupIn matches an "In `<path>`:" file-group header, the group-prefix
// variant CodeRabbit also emits for the same sections.
var reCRGroupIn = regexp.MustCompile("^In `(.+?)`:\\s*$")

// reCRSingle matches a single-line finding header, e.g. "- Line 1154: …".
var reCRSingle = regexp.MustCompile(`^\s*[-*]\s+Line (\d+):`)

// reCRRange matches a range finding header, e.g. "- Around line 679-681: …".
var reCRRange = regexp.MustCompile(`^\s*[-*]\s+Around line (\d+)-(\d+):`)

// crPromptSummaries are the <summary> phrasings CodeRabbit has used for the
// agent-prompt block whose fence carries the "Outside diff comments:"
// section. CodeRabbit rewords them between releases, so every observed
// wording must be accepted; a missing summary line means the review carries
// no agent-prompt block at all.
var crPromptSummaries = []string{
	"Prompt to fix all issues",
	"Prompt to fix review comments",
	"Prompt for all review comments",
	"Prompt for AI Agents",
}

// ParseCoderabbitOutsideDiffFindings extracts the outside-diff findings from a
// CodeRabbit review body into RawComment values. CodeRabbit caps its inline
// comments, so findings that exceed the cap (or target lines outside the diff)
// exist only as text in the review body. Unparseable input yields nil, never
// an error: a missing summary line, fence, or section means the review simply
// carries no outside-diff findings.
func ParseCoderabbitOutsideDiffFindings(review coderabbitReview) []RawComment {
	if review.Body == "" {
		return nil
	}

	lines := strings.Split(review.Body, "\n")

	// Locate the prompt-block <summary> line; the fenced block starts at the
	// first line that is exactly "```" after it and ends at the next such
	// line (or runs to the end of the body when the fence is unclosed).
	summaryIdx := -1
	for i, l := range lines {
		if containsPromptSummary(l) {
			summaryIdx = i
			break
		}
	}
	if summaryIdx == -1 {
		return nil
	}
	blockStart := -1
	for i := summaryIdx + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "```" {
			blockStart = i
			break
		}
	}
	if blockStart == -1 {
		return nil
	}
	blockEnd := len(lines)
	for i := blockStart + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "```" {
			blockEnd = i
			break
		}
	}

	m := reCRSection.FindStringSubmatch(strings.Join(lines[blockStart+1:blockEnd], "\n"))
	if m == nil {
		return nil
	}
	section := strings.Split(m[1], "\n")

	var out []RawComment
	type findingKey struct {
		path      string
		lineStart int
		lineEnd   int
		body      string
	}
	seen := make(map[findingKey]bool)

	var (
		path       string
		havePath   bool
		inFinding  bool
		fStart     int
		fEnd       int
		fTextLines []string
	)
	flush := func() {
		if !inFinding {
			return
		}
		inFinding = false
		text := strings.TrimSpace(strings.Join(fTextLines, "\n"))
		fTextLines = nil
		key := findingKey{path: path, lineStart: fStart, lineEnd: fEnd, body: text}
		if seen[key] {
			return
		}
		seen[key] = true
		summary := review.ID.String() + "\x00" + path + "\x00" +
			fmt.Sprint(fStart) + "\x00" + fmt.Sprint(fEnd) + "\x00" + text
		id := "cr-" + fmt.Sprintf("%x", sha256.Sum256([]byte(summary)))
		p := path // copy: RawComment.Path is a pointer into this value
		lineStart := fStart
		lineEnd := fEnd
		out = append(out, RawComment{
			ID:            id,
			Body:          text,
			Path:          &p,
			Line:          &lineEnd,
			StartLine:     &lineStart,
			Author:        review.User.Login,
			CreatedAt:     review.SubmittedAt,
			IsResolved:    false,
			IsOutdated:    false,
			ReplyToID:     nil,
			ThreadID:      nil,
			ThreadReplies: 0,
		})
	}

	for _, line := range section {
		trimmed := strings.TrimSpace(line)
		if m := reCRGroupAt.FindStringSubmatch(trimmed); m != nil {
			flush()
			path = m[1]
			havePath = true
			continue
		}
		if m := reCRGroupIn.FindStringSubmatch(trimmed); m != nil {
			flush()
			path = strings.TrimPrefix(m[1], "@")
			havePath = true
			continue
		}
		if strings.HasPrefix(trimmed, crEpilogueStart) {
			// The prompt-block epilogue is boilerplate, not a finding.
			flush()
			break
		}
		if ix := reCRSingle.FindStringSubmatchIndex(line); ix != nil {
			flush()
			if havePath {
				n, _ := strconv.Atoi(line[ix[2]:ix[3]])
				inFinding = true
				fStart = n
				fEnd = n
				fTextLines = []string{line[ix[1]:]}
			}
			continue
		}
		if ix := reCRRange.FindStringSubmatchIndex(line); ix != nil {
			flush()
			if havePath {
				start, _ := strconv.Atoi(line[ix[2]:ix[3]])
				end, _ := strconv.Atoi(line[ix[4]:ix[5]])
				inFinding = true
				fStart = start
				fEnd = end
				fTextLines = []string{line[ix[1]:]}
			}
			continue
		}
		if inFinding {
			// Any other line — wrapped text, a non-header bullet inside a
			// finding, or a blank line — is continuation of the current
			// finding.
			fTextLines = append(fTextLines, line)
			continue
		}
	}
	flush()

	return out
}

// containsPromptSummary reports whether line contains any of the observed
// CodeRabbit prompt-block <summary> phrasings (crPromptSummaries).
func containsPromptSummary(line string) bool {
	for _, s := range crPromptSummaries {
		if strings.Contains(line, s) {
			return true
		}
	}
	return false
}

// isCoderabbitAuthor reports whether login is the CodeRabbit bot account
// (exact, case-insensitive match — a human login merely containing the
// substring must not participate in selection).
func isCoderabbitAuthor(login string) bool {
	return strings.EqualFold(login, coderabbitBotLogin)
}

// collectCoderabbitFindings walks the CodeRabbit reviews newest to oldest
// and returns the first review that yields outside-diff findings. CodeRabbit
// posts a new review per commit range, so a newer review's findings supersede
// older ones; body findings carry no outdated flag, so newest-first is the
// staleness guard. Walking (instead of taking only the newest) survives the
// real-world case where the newest bot review is an empty status review
// (e.g. APPROVED with body "") that carries no findings at all.
// Selection compares SubmittedAt instants (never RFC3339 strings — string
// order is not chronological when offsets differ); reviews with a nil or
// unparseable SubmittedAt are not candidates; ties are broken by
// first-in-list. No candidate yields findings yields nil.
func collectCoderabbitFindings(reviews []coderabbitReview) []RawComment {
	type candidate struct {
		index int
		at    time.Time
	}
	var candidates []candidate
	for i, r := range reviews {
		if !isCoderabbitAuthor(r.User.Login) {
			continue
		}
		if r.SubmittedAt == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, *r.SubmittedAt)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{index: i, at: t})
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		return candidates[a].at.After(candidates[b].at)
	})
	for _, c := range candidates {
		if findings := ParseCoderabbitOutsideDiffFindings(reviews[c.index]); findings != nil {
			return findings
		}
	}
	return nil
}

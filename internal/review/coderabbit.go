package review

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// coderabbitAuthorSubstring matches CodeRabbit review authors case-insensitively
// (covers coderabbitai[bot]; a human login containing the substring is an
// accepted edge).
const coderabbitAuthorSubstring = "coderabbit"

// coderabbitReview is the subset of a GitHub/Forgejo PR review this parser
// needs: the body (where CodeRabbit writes its "Prompt to fix all issues"
// block), the submission timestamp, and the author login.
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
var reCRSection = regexp.MustCompile(`(?m)^Outside diff comments:\r?\n([\s\S]*?)(?:\n(?:Minor comments:|Failed pre-merge checks:)|\n---\r?$|\z)`)

// reCRSingle matches a single-line finding header, e.g. "- Line 1154: …".
var reCRSingle = regexp.MustCompile(`^\s*[-*]\s+Line (\d+):`)

// reCRRange matches a range finding header, e.g. "- Around line 679-681: …".
var reCRRange = regexp.MustCompile(`^\s*[-*]\s+Around line (\d+)-(\d+):`)

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

	// Locate the "Prompt to fix all issues" summary line; the fenced block
	// starts at the first line that is exactly "```" after it and ends at the
	// next such line (or runs to the end of the body when the fence is
	// unclosed).
	summaryIdx := -1
	for i, l := range lines {
		if strings.Contains(l, "Prompt to fix all issues") {
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
		if strings.HasPrefix(trimmed, "Review comments at @") {
			flush()
			path = strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(trimmed, "Review comments at @")), ":")
			havePath = true
			continue
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

// collectCoderabbitFindings selects the newest CodeRabbit review among
// reviews and returns its outside-diff findings. CodeRabbit posts a new review
// per commit range, so findings in older reviews are stale; body findings
// carry no outdated flag, so newest-only selection is the staleness guard.
// Selection compares SubmittedAt instants (never RFC3339 strings — string
// order is not chronological when offsets differ); reviews with a nil or
// unparseable SubmittedAt are not candidates; ties are broken by first-in-list.
// No candidates yields nil.
func collectCoderabbitFindings(reviews []coderabbitReview) []RawComment {
	best := -1
	var bestTime time.Time
	for i, r := range reviews {
		if !strings.Contains(strings.ToLower(r.User.Login), coderabbitAuthorSubstring) {
			continue
		}
		if r.SubmittedAt == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, *r.SubmittedAt)
		if err != nil {
			continue
		}
		if best == -1 || t.After(bestTime) {
			best = i
			bestTime = t
		}
	}
	if best == -1 {
		return nil
	}
	return ParseCoderabbitOutsideDiffFindings(reviews[best])
}

package review

import (
	"regexp"
	"strings"
	"time"
)

// SeverityMap maps the leading emoji of a Mira comment's second line to a
// Severity.
var SeverityMap = map[string]Severity{
	"⛔":  SeverityBlocker,
	"🛑":  SeverityBlocker,
	"⚠️": SeverityWarning,
	"💡":  SeveritySuggestion,
	"💬":  SeverityNitpick,
}

// MiraAuthors is the list of author-login substrings that identify Mira bots.
var MiraAuthors = []string{"miracodeai-bot", "miracodeai", "bot-mira"}

// IsMiraComment reports whether the author login looks like a Mira bot.
func IsMiraComment(author string) bool {
	lower := strings.ToLower(author)
	for _, a := range MiraAuthors {
		if strings.Contains(lower, a) {
			return true
		}
	}
	return false
}

// IsTrustedAuthor reports whether author is a trusted source of agent
// instructions: its login ends in "bot" (case-insensitive — covers the
// "-bot" and bare "bot" suffixes, e.g. miracodeai-bot, nimuebot), or it
// appears in trustedAuthors (case-insensitive, exact match after trim).
// An empty login is never trusted. This gates ParsedComment.AgentPrompt so
// arbitrary PR reviewers cannot inject agent instructions.
func IsTrustedAuthor(author string, trustedAuthors ...string) bool {
	lower := strings.ToLower(author)
	if lower == "" {
		return false
	}
	if strings.HasSuffix(lower, "bot") {
		return true
	}
	for _, a := range trustedAuthors {
		if a = strings.TrimSpace(a); a != "" && strings.EqualFold(a, author) {
			return true
		}
	}
	return false
}

var reCategory = regexp.MustCompile(`^\*\*(.+?)\*\*`)

// ParseCategory extracts the category from the first `**bold**` run of the
// first line. Returns "unknown" when no match is found.
func ParseCategory(body string) string {
	lines := strings.Split(body, "\n")
	if len(lines) == 0 {
		return "unknown"
	}
	first := strings.TrimSpace(lines[0])
	m := reCategory.FindStringSubmatch(first)
	if len(m) < 2 {
		return "unknown"
	}
	if v := strings.TrimSpace(m[1]); v != "" {
		return v
	}
	return "unknown"
}

// ParseSeverity reads the emoji prefix of the second line and maps it to a
// Severity. Returns SeveritySuggestion as the default fallback.
func ParseSeverity(body string) Severity {
	lines := strings.Split(body, "\n")
	if len(lines) < 2 {
		return SeveritySuggestion
	}
	sevLine := strings.TrimSpace(lines[1])
	for emoji, sev := range SeverityMap {
		if strings.HasPrefix(sevLine, emoji) {
			return sev
		}
	}
	return SeveritySuggestion
}

// ParseTitle returns the first **bold** line after the category+severity lines
// that is not a "Fix" or "Note" marker. Returns "Untitled" when none is found.
func ParseTitle(body string) string {
	lines := strings.Split(body, "\n")
	for i := 2; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if len(line) >= 4 && strings.HasPrefix(line, "**") && strings.HasSuffix(line, "**") &&
			!strings.HasPrefix(line, "**Fix") && !strings.HasPrefix(line, "**Note") {
			return strings.TrimSpace(line[2 : len(line)-2])
		}
	}
	return "Untitled"
}

var reSuggestion = regexp.MustCompile("```suggestion\n([\\s\\S]*?)```")

// ParseSuggestion extracts the inner text of a ```suggestion fenced block.
// Returns nil when absent.
func ParseSuggestion(body string) *string {
	m := reSuggestion.FindStringSubmatch(body)
	if len(m) < 2 {
		return nil
	}
	v := strings.TrimSpace(m[1])
	return &v
}

var (
	reAgentBlock = regexp.MustCompile(`<details>\s*<summary>Prompt for AI Agents</summary>([\s\S]*?)</details>`)
	reCodeBlock  = regexp.MustCompile("```\n?([\\s\\S]*?)```")
)

// ParseAgentPrompt extracts the "Prompt for AI Agents" details block, preferring
// the inner fenced code block's content. Returns nil when absent.
func ParseAgentPrompt(body string) *string {
	block := reAgentBlock.FindStringSubmatch(body)
	if len(block) < 2 {
		return nil
	}
	inner := block[1]
	code := reCodeBlock.FindStringSubmatch(inner)
	var v string
	if len(code) >= 2 {
		v = code[1]
	} else {
		v = inner
	}
	v = strings.TrimSpace(v)
	return &v
}

var (
	reStripSuggestion = regexp.MustCompile("```suggestion[\\s\\S]*?```")
	reStripAgent      = regexp.MustCompile(`<details>\s*<summary>Prompt for AI Agents</summary>[\s\S]*?</details>`)
	reStripFooter     = regexp.MustCompile(`(?s)> Not useful\?.*$`)
	reStripSeparator  = regexp.MustCompile(`(?m)^---$`)
)

// ParseBody strips suggestion blocks, agent-prompt details blocks, the "Not
// useful?" footer, and standalone separators from the raw body, returning the
// trimmed remainder.
func ParseBody(body string) string {
	cleaned := reStripSuggestion.ReplaceAllString(body, "")
	cleaned = reStripAgent.ReplaceAllString(cleaned, "")
	cleaned = reStripFooter.ReplaceAllString(cleaned, "")
	cleaned = reStripSeparator.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

// firstNonNilInt returns the first non-nil pointer, or nil.
func firstNonNilInt(a, b *int) *int {
	if a != nil {
		return a
	}
	return b
}

// FallbackID returns id as-is when non-empty, or "?" as a fallback.
func FallbackID(id string) string {
	if id == "" {
		return "?"
	}
	return id
}

// ParseComment assembles a ParsedComment from a RawComment, deriving all
// parsed fields from the body and applying path/line fallbacks.
//
// Trust: the AgentPrompt field carries agent instructions embedded in the
// comment body. Because root comments from ANY author are included by
// default, a prompt is only emitted when the author is trusted (bot-suffixed
// login); otherwise AgentPrompt is nil and IsTrusted is false so consumers
// can see why the prompt is absent.
func ParseComment(comment RawComment) ParsedComment {
	return ParseCommentTrusted(comment, nil)
}

// ParseCommentTrusted is ParseComment with an explicit trusted-authors list
// (e.g. the --trusted-authors flag): an author is trusted when the login ends
// in "bot" (case-insensitive) or appears in trustedAuthors. The AgentPrompt
// is emitted only for trusted authors; IsTrusted records the classification.
func ParseCommentTrusted(comment RawComment, trustedAuthors []string) ParsedComment {
	file := "unknown"
	if comment.Path != nil && *comment.Path != "" {
		file = *comment.Path
	}

	startLine := firstNonNilInt(comment.StartLine, comment.Line)
	endLine := firstNonNilInt(comment.Line, comment.StartLine)

	lineStart := 0
	if startLine != nil {
		lineStart = *startLine
	}
	lineEnd := 0
	if endLine != nil {
		lineEnd = *endLine
	}

	trusted := IsTrustedAuthor(comment.Author, trustedAuthors...)
	agentPrompt := ParseAgentPrompt(comment.Body)
	if !trusted {
		agentPrompt = nil
	}

	return ParsedComment{
		ID:            comment.ID,
		File:          file,
		LineStart:     lineStart,
		LineEnd:       lineEnd,
		Category:      ParseCategory(comment.Body),
		Severity:      ParseSeverity(comment.Body),
		Title:         ParseTitle(comment.Body),
		Author:        comment.Author,
		IsMira:        IsMiraComment(comment.Author),
		IsTrusted:     trusted,
		Body:          ParseBody(comment.Body),
		Suggestion:    ParseSuggestion(comment.Body),
		AgentPrompt:   agentPrompt,
		DiffHunk:      comment.DiffHunk,
		IsResolved:    comment.IsResolved,
		IsOutdated:    comment.IsOutdated,
		CreatedAt:     comment.CreatedAt,
		ThreadID:      comment.ThreadID,
		ThreadReplies: comment.ThreadReplies,
	}
}

// FilterRootComments keeps root comments (no reply parent). When authors is
// empty, root comments from ANY author are kept. Otherwise, only root comments
// whose author matches one of the listed logins (case-insensitive exact match)
// are kept.
func FilterRootComments(comments []RawComment, authors ...string) []RawComment {
	allow := make(map[string]struct{}, len(authors))
	for _, a := range authors {
		if a = strings.TrimSpace(a); a != "" {
			allow[strings.ToLower(a)] = struct{}{}
		}
	}
	out := make([]RawComment, 0, len(comments))
	for _, c := range comments {
		if c.ReplyToID != nil {
			continue
		}
		if len(allow) == 0 {
			out = append(out, c)
			continue
		}
		if _, ok := allow[strings.ToLower(c.Author)]; ok {
			out = append(out, c)
		}
	}
	return out
}

// FilterOutAuthors drops comments whose author matches any of the given logins
// (case-insensitive). An empty authors list is the identity function.
func FilterOutAuthors(comments []RawComment, authors ...string) []RawComment {
	deny := make(map[string]struct{}, len(authors))
	for _, a := range authors {
		if a = strings.TrimSpace(a); a != "" {
			deny[strings.ToLower(a)] = struct{}{}
		}
	}
	if len(deny) == 0 {
		return comments
	}
	out := make([]RawComment, 0, len(comments))
	for _, c := range comments {
		if _, ok := deny[strings.ToLower(c.Author)]; ok {
			continue
		}
		out = append(out, c)
	}
	return out
}

// FilterSince keeps comments created strictly after since. Comments with a
// missing or unparseable CreatedAt are KEPT (fail-open) so an undatable
// comment is never silently hidden.
func FilterSince(comments []RawComment, since time.Time) []RawComment {
	out := make([]RawComment, 0, len(comments))
	for _, c := range comments {
		if c.CreatedAt == nil {
			out = append(out, c)
			continue
		}
		ts, err := time.Parse(time.RFC3339, *c.CreatedAt)
		if err != nil {
			out = append(out, c)
			continue
		}
		if ts.After(since) {
			out = append(out, c)
		}
	}
	return out
}

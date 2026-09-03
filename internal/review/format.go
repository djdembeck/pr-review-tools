package review

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// FormatJSON serializes the parsed comments as indented JSON. An empty slice is
// rendered as `[]` (never `null`). HTML escaping is disabled so output matches
// JS JSON.stringify byte-for-byte (e.g. `<` stays `<`, not `\u003c`).
func FormatJSON(comments []ParsedComment) (string, error) {
	if comments == nil {
		comments = []ParsedComment{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(comments); err != nil {
		return "", fmt.Errorf("marshal JSON: %w", err)
	}
	// json.Encoder.Encode appends a trailing newline; trim it for parity with
	// TS JSON.stringify which does not.
	return strings.TrimRight(buf.String(), "\n"), nil
}

// FormatConsensus renders a deterministic markdown summary grouped by file
// (alphabetically sorted). Files are sorted to guarantee stable LLM output, a
// deliberate improvement over the TS Map insertion order.
func FormatConsensus(comments []ParsedComment) string {
	if len(comments) == 0 {
		return "No Mira review comments found."
	}

	byFile := make(map[string][]ParsedComment)
	for _, c := range comments {
		byFile[c.File] = append(byFile[c.File], c)
	}

	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	noun := "comments"
	if len(comments) == 1 {
		noun = "comment"
	}
	lines := []string{
		fmt.Sprintf("# Mira Review — %d %s", len(comments), noun),
		"",
	}

	for _, file := range files {
		fileComments := byFile[file]
		lines = append(lines, fmt.Sprintf("## `%s`", file), "")
		for _, c := range fileComments {
			status := "❌ Open"
			if c.IsResolved {
				status = "✅ Resolved"
			}
			lineRange := fmt.Sprintf("L%d", c.LineStart)
			if c.LineEnd != c.LineStart {
				lineRange = fmt.Sprintf("L%d-%d", c.LineStart, c.LineEnd)
			}
			lines = append(lines, fmt.Sprintf("### %s — %s", lineRange, c.Title), "")

			bodyLines := strings.Split(c.Body, "\n")
			if len(bodyLines) > 3 {
				bodyLines = bodyLines[:3]
			}
			quoted := strings.Join(bodyLines, "\n> ")
			lines = append(lines,
				fmt.Sprintf("> **%s** | %s | %s", c.Category, strings.ToUpper(string(c.Severity)), status),
				">",
				"> "+quoted,
				"",
			)

			if c.Suggestion != nil {
				lines = append(lines,
					"**Suggested fix:**",
					"```suggestion",
					*c.Suggestion,
					"```",
					"",
				)
			}
			if c.AgentPrompt != nil {
				lines = append(lines,
					"<details><summary>Agent prompt</summary>",
					"",
					"```",
					*c.AgentPrompt,
					"```",
					"",
					"</details>",
					"",
				)
			}
			if c.DiffHunk != nil {
				hunkLines := strings.Split(*c.DiffHunk, "\n")
				if len(hunkLines) > 15 {
					hunkLines = hunkLines[:15]
				}
				lines = append(lines,
					"<details><summary>Diff context</summary>",
					"",
					"```diff",
					strings.Join(hunkLines, "\n"),
					"```",
					"",
					"</details>",
					"",
				)
			}

			lines = append(lines, "---", "")
		}
	}

	return strings.Join(lines, "\n")
}

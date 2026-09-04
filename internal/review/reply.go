package review

import "fmt"

// BuildRejectBody constructs the reply body that triggers Mira's learning loop:
// `@<botName> reject — <reason>`. When commitHash is non-empty, an audit note
// referencing the commit is appended.
func BuildRejectBody(botName, reason, commitHash string) string {
	body := fmt.Sprintf("@%s reject — %s", botName, reason)
	if commitHash != "" {
		body += fmt.Sprintf("\n\n_Reasoning recorded for audit. See commit %s._", commitHash)
	}
	return body
}

// BuildAcknowledgeBody constructs the reply body acknowledging a valid finding.
// When commitHash is provided it references the fixing commit; otherwise it
// indicates the fix is in progress on the current branch. A note may be
// appended.
func BuildAcknowledgeBody(commitHash, note string) string {
	if commitHash != "" {
		s := fmt.Sprintf("Acknowledged — valid finding. Fixed in commit %s.", commitHash)
		if note != "" {
			s += " " + note
		}
		return s
	}
	s := "Acknowledged — valid finding. Fixing in this branch."
	if note != "" {
		s += " " + note
	}
	return s
}

package review

import (
	"strings"
	"testing"
)

// TestBuildAcknowledgeBody pins the static-template + appended-note contract
// that the fixup workflow relies on: the body carries exactly ONE
// "Acknowledged" sentence from the template, and the note (when present) is
// appended after it. Callers that pass an acknowledgement as the note
// produce a duplicated acknowledgment — the template must stay static.
func TestBuildAcknowledgeBody(t *testing.T) {
	tests := []struct {
		name       string
		commitHash string
		note       string
		want       string
	}{
		{
			name: "no commit no note",
			want: "Acknowledged — valid finding. Fixing in this branch.",
		},
		{
			name:       "commit no note",
			commitHash: "abc123",
			want:       "Acknowledged — valid finding. Fixed in commit abc123.",
		},
		{
			name: "note appends after template",
			note: "partial: stdlib leaf-symlink hardening; residual parent-dir window documented",
			want: "Acknowledged — valid finding. Fixing in this branch. partial: stdlib leaf-symlink hardening; residual parent-dir window documented",
		},
		{
			name:       "commit and note append",
			commitHash: "abc123",
			note:       "covered by downloader tests",
			want:       "Acknowledged — valid finding. Fixed in commit abc123. covered by downloader tests",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildAcknowledgeBody(tt.commitHash, tt.note)
			if got != tt.want {
				t.Fatalf("BuildAcknowledgeBody(%q, %q) = %q, want %q", tt.commitHash, tt.note, got, tt.want)
			}
			if strings.Count(got, "Acknowledged") != 1 {
				t.Fatalf("BuildAcknowledgeBody(%q, %q) contains %d acknowledgments, want 1: %q", tt.commitHash, tt.note, strings.Count(got, "Acknowledged"), got)
			}
		})
	}
}

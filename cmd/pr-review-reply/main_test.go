package main

import (
	"strings"
	"testing"
)

// TestParseArgsValue covers the parse-time validation contract:
// value-taking options must never consume a flag-shaped token as their
// value, and at most one primary action flag may be given per invocation.
// All cases must be rejected inside parseArgsValue, i.e. before any API
// operation runs.
func TestParseArgsValue(t *testing.T) {
	tests := []struct {
		name    string
		argv    []string
		wantErr string // substring expected in the error; "" means success
		want    args   // expected parse result when wantErr == ""
	}{
		{
			name:    "flag-shaped token consumed as reply ID is rejected",
			argv:    []string{"pr-review-reply", "12", "--reply", "123", "--body", "--dry-run"},
			wantErr: `Error: --body requires a value; "--dry-run" looks like a flag`,
		},
		{
			name:    "flag-shaped token as reply ID is rejected",
			argv:    []string{"pr-review-reply", "12", "--reply", "--and-resolve"},
			wantErr: `Error: --reply requires a value; "--and-resolve" looks like a flag`,
		},
		{
			name:    "flag-shaped token as resolve ID is rejected",
			argv:    []string{"pr-review-reply", "12", "--resolve", "--dry-run"},
			wantErr: `Error: --resolve requires a value; "--dry-run" looks like a flag`,
		},
		{
			name:    "flag-shaped token as reason is rejected",
			argv:    []string{"pr-review-reply", "12", "--reject", "456", "--reason", "--dry-run"},
			wantErr: `Error: --reason requires a value; "--dry-run" looks like a flag`,
		},
		{
			name:    "flag-shaped token as batch file path is rejected",
			argv:    []string{"pr-review-reply", "12", "--batch-reply", "--dry-run"},
			wantErr: `Error: --batch-reply requires a value; "--dry-run" looks like a flag`,
		},
		{
			name:    "value missing at end of args is rejected",
			argv:    []string{"pr-review-reply", "12", "--reply", "123", "--body"},
			wantErr: "Error: --body requires a value",
		},
		{
			name:    "action ID missing at end of args is rejected",
			argv:    []string{"pr-review-reply", "12", "--resolve"},
			wantErr: "Error: --resolve requires a value",
		},
		{
			name:    "conflicting primary actions reply and resolve",
			argv:    []string{"pr-review-reply", "12", "--reply", "123", "--body", "fixed", "--resolve", "456"},
			wantErr: "one action per invocation; both --reply and --resolve were given",
		},
		{
			name:    "conflicting primary actions reject and acknowledge",
			argv:    []string{"pr-review-reply", "12", "--reject", "123", "--reason", "no", "--acknowledge", "456"},
			wantErr: "one action per invocation; both --reject and --acknowledge were given",
		},
		{
			name:    "conflicting primary actions resolve and detect-bot",
			argv:    []string{"pr-review-reply", "12", "--resolve", "123", "--detect-bot"},
			wantErr: "one action per invocation; both --resolve and --detect-bot were given",
		},
		{
			name:    "alias ack conflicts with reply and reports canonical name",
			argv:    []string{"pr-review-reply", "12", "--reply", "123", "--body", "fixed", "--ack", "456"},
			wantErr: "one action per invocation; both --reply and --acknowledge were given",
		},
		{
			name:    "alias batch-ack conflicts with reply and reports canonical name",
			argv:    []string{"pr-review-reply", "12", "--reply", "123", "--body", "x", "--batch-ack", "acks.json"},
			wantErr: "one action per invocation; both --reply and --batch-acknowledge were given",
		},
		{
			name: "alias ack alone parses to acknowledge",
			argv: []string{"pr-review-reply", "7", "--ack", "55", "--note", "lgtm"},
			want: args{
				prNumber:  7,
				act:       actionAcknowledge,
				commentID: "55",
				note:      "lgtm",
			},
		},
		{
			name: "alias batch-ack alone parses to batch-acknowledge",
			argv: []string{"pr-review-reply", "7", "--batch-ack", "acks.json", "--format", "json"},
			want: args{
				prNumber:   7,
				act:        actionBatchAcknowledge,
				batchFile:  "acks.json",
				formatJSON: true,
			},
		},
		{
			name: "valid reply with and-resolve modifier",
			argv: []string{"pr-review-reply", "12", "--reply", "123", "--body", "fixed", "--and-resolve", "--dry-run"},
			want: args{
				prNumber:   12,
				act:        actionReply,
				commentID:  "123",
				body:       "fixed",
				andResolve: true,
				dryRun:     true,
			},
		},
		{
			name: "valid reject",
			argv: []string{"pr-review-reply", "7", "--reject", "99", "--reason", "broken"},
			want: args{
				prNumber:  7,
				act:       actionReject,
				commentID: "99",
				reason:    "broken",
			},
		},
		{
			name: "valid acknowledge with commit",
			argv: []string{"pr-review-reply", "7", "--ack", "55", "--commit", "abc123"},
			want: args{
				prNumber:   7,
				act:        actionAcknowledge,
				commentID:  "55",
				commitHash: "abc123",
			},
		},
		{
			name: "valid resolve",
			argv: []string{"pr-review-reply", "7", "--resolve", "42"},
			want: args{
				prNumber:      7,
				act:           actionResolve,
				resolveTarget: "42",
			},
		},
		{
			name: "valid batch reply",
			argv: []string{"pr-review-reply", "7", "--batch-reply", "replies.json", "--format", "json"},
			want: args{
				prNumber:   7,
				act:        actionBatchReply,
				batchFile:  "replies.json",
				formatJSON: true,
			},
		},
		{
			name: "valid detect-bot",
			argv: []string{"pr-review-reply", "7", "--detect-bot"},
			want: args{
				prNumber: 7,
				act:      actionDetectBot,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err, showHelp := parseArgsValue(tt.argv)
			if showHelp {
				t.Fatalf("parseArgsValue(%v) = help, want %s", tt.argv, tt.wantErr)
			}
			if tt.wantErr == "" {
				if err != "" {
					t.Fatalf("parseArgsValue(%v) unexpected error: %s", tt.argv, err)
				}
				if got != tt.want {
					t.Fatalf("parseArgsValue(%v) = %+v, want %+v", tt.argv, got, tt.want)
				}
				return
			}
			if !strings.Contains(err, tt.wantErr) {
				t.Fatalf("parseArgsValue(%v) error = %q, want it to contain %q", tt.argv, err, tt.wantErr)
			}
		})
	}
}

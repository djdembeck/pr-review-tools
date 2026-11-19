//go:build !unix

package review

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStateRefusesOnNonUnixPlatforms proves the fail-closed stance on
// platforms without POSIX ownership semantics: even a well-formed, valid
// state file written by this process is refused on load, because the owner
// cannot be verified. This is the opposite of the unix path, where a file
// owned by the current uid resumes normally.
func TestStateRefusesOnNonUnixPlatforms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := NewState(StateBinding{
		Platform: PlatformGitHub, Repo: "owner/repo", PR: 1, Head: testHead,
		WaitFor: WaitForAll, Authors: []string{"alice"},
	}, nil, now)
	st.BaselineComplete = true
	if err := SaveState(path, st, now); err != nil {
		t.Fatalf("save: %v", err)
	}
	_, loaded, err := LoadState(path)
	if loaded {
		t.Fatal("state loaded on a platform without ownership semantics; must be refused")
	}
	if err == nil {
		t.Fatal("LoadState returned nil error for an unverifiable state file")
	}
	var se *StateError
	if !errors.As(err, &se) {
		t.Fatalf("want StateError, got %T: %v", err, err)
	}
	if !strings.Contains(se.Msg, "platform") {
		t.Fatalf("StateError must name the unsupported platform, got: %q", se.Msg)
	}
}

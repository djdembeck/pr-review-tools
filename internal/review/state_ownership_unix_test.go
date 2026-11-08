//go:build unix

package review

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCheckStateOwnershipFailClosed proves the two refusal branches of
// checkStateOwnership, forced through the statSys and currentUID seams so
// they run unconditionally on any unix host: a uid mismatch is refused, and a
// stat result whose owner cannot be read (!ok) is also refused — never
// skipped.
func TestCheckStateOwnershipFailClosed(t *testing.T) {
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

	cases := []struct {
		name    string
		mutate  func(t *testing.T)
		wantMsg string
	}{
		{
			name: "uid mismatch is refused",
			mutate: func(t *testing.T) {
				uid := currentUID()
				currentUID = func() int { return uid + 1 }
				t.Cleanup(func() { currentUID = os.Getuid })
			},
			wantMsg: "owned by uid",
		},
		{
			name: "unverifiable owner is refused",
			mutate: func(t *testing.T) {
				orig := statSys
				statSys = func(os.FileInfo) (uint32, bool) { return 0, false }
				t.Cleanup(func() { statSys = orig })
			},
			wantMsg: "cannot verify",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			tt.mutate(t)
			_, loaded, err := LoadState(path)
			if loaded {
				t.Fatal("state loaded; the ownership refusal did not fire")
			}
			var se *StateError
			if !errors.As(err, &se) {
				t.Fatalf("want StateError, got %T: %v", err, err)
			}
			if !strings.Contains(se.Msg, tt.wantMsg) {
				t.Fatalf("StateError message = %q, want it to contain %q", se.Msg, tt.wantMsg)
			}
		})
	}
}

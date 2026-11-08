package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StateVersion is the on-disk lifecycle state schema version. A state file
// written by another version is refused, never upgraded in place.
const StateVersion = 1

// State is the durable, owner-controlled cursor for one lifecycle watch round.
// It is bound to a single platform/repo/PR/head and to the exact selected
// author set and wait mode; any mismatch is an error, never a silent reset.
// It stores evidence and cursor data only: no tokens, no headers, no raw auth
// responses.
type State struct {
	Version          int             `json:"version"`
	Platform         Platform        `json:"platform"`
	Repo             string          `json:"repo"`
	PR               int             `json:"pr"`
	Head             string          `json:"head"`
	WaitFor          string          `json:"waitFor"`
	Authors          []string        `json:"authors"`
	TrustedAuthors   []string        `json:"trustedAuthors,omitempty"`
	BaselineComplete bool            `json:"baselineComplete"`
	BaselineAt       string          `json:"baselineAt,omitempty"`
	Events           []Event         `json:"events"`
	Seen             map[string]bool `json:"seen"`
	Polls            int             `json:"polls"`
	CreatedAt        string          `json:"createdAt,omitempty"`
	UpdatedAt        string          `json:"updatedAt,omitempty"`
}

// StateBinding is the identity a state file must match to be reused.
type StateBinding struct {
	Platform Platform
	Repo     string
	PR       int
	Head     string
	WaitFor  string
	Authors  []string
}

// StateError reports a durable-state problem that must abort the round. It is
// always fatal: the contract forbids resetting mismatched or malformed state.
type StateError struct {
	Path string
	Msg  string
}

func (e *StateError) Error() string {
	if e.Path == "" {
		return "review state: " + e.Msg
	}
	return fmt.Sprintf("review state %s: %s", e.Path, e.Msg)
}

// NewState builds an empty state bound to the given identity.
func NewState(bind StateBinding, trusted []string, now time.Time) *State {
	stamp := now.UTC().Format(time.RFC3339Nano)
	return &State{
		Version:        StateVersion,
		Platform:       bind.Platform,
		Repo:           bind.Repo,
		PR:             bind.PR,
		Head:           bind.Head,
		WaitFor:        bind.WaitFor,
		Authors:        append([]string(nil), bind.Authors...),
		TrustedAuthors: append([]string(nil), trusted...),
		Events:         []Event{},
		Seen:           map[string]bool{},
		CreatedAt:      stamp,
		UpdatedAt:      stamp,
	}
}

// LoadState reads and validates a state file. A missing file is reported with
// the returned bool as false and a nil error so the caller can initialize.
func LoadState(path string) (*State, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, &StateError{Path: path, Msg: err.Error()}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, &StateError{Path: path, Msg: "is a symlink; refusing to follow an owner-controlled state path through a link"}
	}
	if !info.Mode().IsRegular() {
		return nil, false, &StateError{Path: path, Msg: "is not a regular file"}
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, false, &StateError{Path: path, Msg: fmt.Sprintf("has mode %04o; refusing a group/other-writable state file", info.Mode().Perm())}
	}
	if err := checkStateOwnership(path, info); err != nil {
		return nil, false, err
	}

	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, false, &StateError{Path: path, Msg: err.Error()}
	}
	var st State
	if err := json.Unmarshal(payload, &st); err != nil {
		return nil, false, &StateError{Path: path, Msg: "malformed JSON (refusing to reset): " + err.Error()}
	}
	if st.Version != StateVersion {
		return nil, false, &StateError{Path: path, Msg: fmt.Sprintf("unsupported schema version %d (want %d); start a new state path", st.Version, StateVersion)}
	}
	if st.Seen == nil {
		st.Seen = map[string]bool{}
	}
	if !st.BaselineComplete {
		return nil, false, &StateError{Path: path, Msg: "baseline incomplete; refusing to resume a partially initialized round"}
	}
	return &st, true, nil
}

// currentUID reports the uid of the running process. It is a variable, not a
// direct call, so tests can force an owner mismatch without chowning a file.
var currentUID = os.Getuid

// ValidateBinding reports whether the loaded state belongs to the requested
// round identity.
func (s *State) ValidateBinding(bind StateBinding) error {
	var problems []string
	if s.Platform != bind.Platform {
		problems = append(problems, fmt.Sprintf("platform %q != %q", s.Platform, bind.Platform))
	}
	if !strings.EqualFold(s.Repo, bind.Repo) {
		problems = append(problems, fmt.Sprintf("repo %q != %q", s.Repo, bind.Repo))
	}
	if s.PR != bind.PR {
		problems = append(problems, fmt.Sprintf("pr %d != %d", s.PR, bind.PR))
	}
	if !strings.EqualFold(s.Head, bind.Head) {
		problems = append(problems, fmt.Sprintf("head %q != %q", s.Head, bind.Head))
	}
	if !strings.EqualFold(s.WaitFor, bind.WaitFor) {
		problems = append(problems, fmt.Sprintf("wait-for %q != %q", s.WaitFor, bind.WaitFor))
	}
	if !sameAuthorSet(s.Authors, bind.Authors) {
		problems = append(problems, fmt.Sprintf("authors %v != %v", s.Authors, bind.Authors))
	}
	if len(problems) > 0 {
		return &StateError{Msg: "does not match this round (" + strings.Join(problems, "; ") + "); start a new state path for a different round"}
	}
	return nil
}

// sameAuthorSet compares author lists as case-insensitive sets, so a
// re-ordered or re-cased CSV is the same round but an added or removed login
// is not.
func sameAuthorSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, x := range a {
		seen[strings.ToLower(strings.TrimSpace(x))]++
	}
	for _, x := range b {
		key := strings.ToLower(strings.TrimSpace(x))
		seen[key]--
		if seen[key] < 0 {
			return false
		}
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// SaveState writes the state atomically: temp file in the same directory,
// fsync, then rename over the target. The file is created 0600 so no other
// user can rewrite the cursor.
func SaveState(path string, st *State, now time.Time) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return &StateError{Path: path, Msg: "create state directory: " + err.Error()}
	}
	st.Version = StateVersion
	st.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	if st.Seen == nil {
		st.Seen = map[string]bool{}
	}
	if st.Events == nil {
		st.Events = []Event{}
	}

	payload, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return &StateError{Path: path, Msg: "marshal: " + err.Error()}
	}
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return &StateError{Path: path, Msg: "create temp file: " + err.Error()}
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup on the failure paths; a successful rename has
		// already moved the file away.
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return &StateError{Path: path, Msg: "chmod temp file: " + err.Error()}
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return &StateError{Path: path, Msg: "write temp file: " + err.Error()}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return &StateError{Path: path, Msg: "sync temp file: " + err.Error()}
	}
	if err := tmp.Close(); err != nil {
		return &StateError{Path: path, Msg: "close temp file: " + err.Error()}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return &StateError{Path: path, Msg: "rename into place: " + err.Error()}
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

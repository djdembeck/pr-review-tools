//go:build unix

package review

import (
	"fmt"
	"os"
	"syscall"
)

// statSys extracts the owning uid from a stat result. It is a variable, not a
// direct assertion, so tests can force the fail-closed !ok path without a
// filesystem whose Sys() type is not *syscall.Stat_t.
var statSys = func(info os.FileInfo) (uid uint32, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

// checkStateOwnership refuses a state file owned by another user. The file
// mode is not enough: a file owned by someone else can still be read and
// reused as this user's cursor. The check is fail-closed: if the owner cannot
// be read from the stat result, resuming is refused, never skipped.
func checkStateOwnership(path string, info os.FileInfo) error {
	uid, ok := statSys(info)
	if !ok {
		return &StateError{Path: path, Msg: "cannot verify file ownership; refusing to resume an owner-controlled state file"}
	}
	cur := currentUID()
	if uid != uint32(cur) {
		return &StateError{Path: path, Msg: fmt.Sprintf("is owned by uid %d, not uid %d; refusing a state file written by another user", uid, cur)}
	}
	return nil
}

//go:build !unix

package review

import "os"

// checkStateOwnership always refuses on platforms without POSIX ownership
// semantics: resuming a state file whose owner cannot be verified would let
// a co-tenant of the host plant a cursor, and this platform has no reliable
// ownership check to rule that out. State resume is unsupported here; start
// a fresh round with a new state path.
func checkStateOwnership(path string, _ os.FileInfo) error {
	return &StateError{Path: path, Msg: "state resume is unsupported on this platform: file ownership cannot be verified, and an owner-controlled state file must be read only by its owner"}
}

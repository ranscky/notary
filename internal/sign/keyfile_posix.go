//go:build !windows

package sign

import (
	"fmt"
	"os"
)

// keyFilePerm is the only acceptable permission mode for a key file on POSIX
// systems.
const keyFilePerm = os.FileMode(0o600)

// checkKeyFileAccess reports whether the key file at path is private enough to
// hold key material, returning a wrapped ErrLoosePerms when it is not.
//
// On POSIX systems the answer is the file's mode: exactly 0600. The comparison
// is exact rather than a bit test, because a mode this project never writes is a
// mode it does not understand -- and 0400, say, would fail later at a worse
// place than here, when the ledger tried to reload the key.
func checkKeyFileAccess(path string, info os.FileInfo) error {
	if perm := info.Mode().Perm(); perm != keyFilePerm {
		return fmt.Errorf("%w: %s has mode %04o", ErrLoosePerms, path, perm)
	}
	return nil
}

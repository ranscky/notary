//go:build !windows

package sign_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// makeKeyFileLoose widens a key file's access so that someone other than its
// owner can read it -- the condition ErrLoosePerms exists to catch.
//
// What "loose" means is platform-specific, and so is this helper. On POSIX the
// mode is the whole story, so widening it to 0644 is enough: group and others
// may read the key, and loadFromFile must refuse it.
func makeKeyFileLoose(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.Chmod(path, 0o644))
}

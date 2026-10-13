//go:build windows

package sign_test

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/sign"
)

// makeKeyFileLoose widens a key file's access so that someone other than its
// owner can read it -- the condition ErrLoosePerms exists to catch.
//
// Windows has no mode to widen: os.Chmod there only toggles the read-only
// attribute, which is not an access control at all. So the access is widened
// where it actually lives, in an ACE granting Everyone read.
//
// The grant names the well-known SID *S-1-1-0 rather than the display name
// "Everyone". The name is localized -- on a non-English Windows it would not
// resolve -- whereas the SID is not, so neither caller can fail for a reason
// that has nothing to do with the code under test.
//
// icacls ships with Windows. If it is missing the file cannot be made loose, and
// that is a failure rather than a skip: every caller needs the file genuinely
// loose to mean what it says, so passing quietly here would turn the check under
// test into one that verified nothing while reporting success.
func makeKeyFileLoose(t *testing.T, path string) {
	t.Helper()
	icacls, err := exec.LookPath("icacls")
	require.NoErrorf(t, err, "icacls is required to make a key file readable by Everyone")
	out, err := exec.Command(icacls, path, "/grant", "*S-1-1-0:(R)").CombinedOutput()
	require.NoErrorf(t, err, "granting Everyone read access to %s: %s", path, out)
}

// TestNewSignerLoadsAnOwnerRestrictedKeyFileOnWindows pins the Windows rule for
// KeySourceFile. Windows has no POSIX permission bits -- os.FileMode.Perm
// reports 0666 for every file, whatever was requested -- so the question there
// is not the mode but the ACL: who besides the owner may read the file. A file
// created into this test's own temp directory inherits access for exactly
// SYSTEM, Administrators and the owner and for nothing else, which is as
// private as a 0600 file on POSIX. It must load.
//
// This is the test the ACL check was written for: before it existed, this
// failed with ErrLoosePerms, because the POSIX mode comparison refused every
// file on Windows.
func TestNewSignerLoadsAnOwnerRestrictedKeyFileOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	require.NoError(t, os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(newSeed(t))), 0o600))

	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceFile, Ref: path})
	require.NoError(t, err, "an owner-restricted key file must load on Windows")
	assert.NotEmpty(t, sg.KeyID())
}

// TestNewSignerRefusesAWorldReadableKeyFileOnWindows is the other half, and the
// reason the Windows check is not simply "skip the check there": a file that
// Everyone may read is no more private than a 0644 file is on POSIX, and it
// must be refused with the same ErrLoosePerms the POSIX check returns, so one
// rule has one sentinel on every platform.
func TestNewSignerRefusesAWorldReadableKeyFileOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	require.NoError(t, os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(newSeed(t))), 0o600))

	// The same file loads while its ACL is still owner-restricted, so the
	// refusal asserted below cannot be an artifact of something else being
	// wrong with it.
	_, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceFile, Ref: path})
	require.NoError(t, err, "precondition: the untouched key file must load")

	makeKeyFileLoose(t, path)

	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceFile, Ref: path})
	require.Error(t, err, "a key file that Everyone may read must be refused")
	assert.ErrorIs(t, err, sign.ErrLoosePerms)
	assert.Nil(t, sg)
}

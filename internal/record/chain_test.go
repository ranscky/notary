package record_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/record"
)

// mustParse parses an RFC3339 timestamp or fails the test.
func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err)
	return ts
}

// TestGenesisHash verifies that the genesis link is exactly 32 zero bytes, so
// the first record's PrevHash is a fixed, unforgeable sentinel.
func TestGenesisHashIs32ZeroBytes(t *testing.T) {
	assert.Equal(t, record.Hash{}, record.GenesisHash())
	assert.Equal(t, 32, len(record.GenesisHash()))
}

// TestComputeHashStable verifies ComputeHash is a pure function of the record:
// repeated calls on an equal record return byte-identical digests.
func TestComputeHashStable(t *testing.T) {
	r := populatedRecord(t)
	h1, err := record.ComputeHash(r)
	require.NoError(t, err)
	h2, err := record.ComputeHash(r)
	require.NoError(t, err)
	assert.Equal(t, h1, h2)
}

// TestComputeHashIncludesChainPosition verifies that Seq and PrevHash are
// mixed into the digest separately from CanonicalBytes: two records identical
// in every canonical field but differing in chain position must hash
// differently. This is what makes a moved record detectable.
func TestComputeHashIncludesChainPosition(t *testing.T) {
	base := populatedRecord(t)

	t.Run("Seq changes the hash", func(t *testing.T) {
		a := base
		b := base
		a.Seq = 1
		b.Seq = 2
		ha, err := record.ComputeHash(a)
		require.NoError(t, err)
		hb, err := record.ComputeHash(b)
		require.NoError(t, err)
		assert.NotEqual(t, ha, hb)
	})

	t.Run("PrevHash changes the hash", func(t *testing.T) {
		a := base
		b := base
		a.PrevHash = record.Hash{0: 1}
		b.PrevHash = record.Hash{0: 2}
		ha, err := record.ComputeHash(a)
		require.NoError(t, err)
		hb, err := record.ComputeHash(b)
		require.NoError(t, err)
		assert.NotEqual(t, ha, hb)
	})
}

// TestComputeHashExcludesHashAndSignature verifies that the two fields the
// record is the *output* of are not part of the input: mutating Hash or
// Signature on the input must leave the computed digest unchanged. Including
// either would be circular.
func TestComputeHashExcludesHashAndSignature(t *testing.T) {
	base := populatedRecord(t)
	baseHash, err := record.ComputeHash(base)
	require.NoError(t, err)

	t.Run("Hash on the input is ignored", func(t *testing.T) {
		mutated := base
		mutated.Hash = record.Hash{0xAA}
		h, err := record.ComputeHash(mutated)
		require.NoError(t, err)
		assert.Equal(t, baseHash, h)
	})

	t.Run("Signature on the input is ignored", func(t *testing.T) {
		mutated := base
		mutated.Signature = []byte("a signature that must not be hashed")
		h, err := record.ComputeHash(mutated)
		require.NoError(t, err)
		assert.Equal(t, baseHash, h)
	})
}

// TestCanonicalBytesNilVsEmptyContent verifies the presence flag: a record with
// no content and a record with empty content are different claims and must
// encode to different bytes.
func TestCanonicalBytesNilVsEmptyContent(t *testing.T) {
	nilContent := populatedRecord(t)
	nilContent.Content = nil

	emptyContent := populatedRecord(t)
	emptyContent.Content = &record.Content{}

	nilBytes, err := record.CanonicalBytes(nilContent)
	require.NoError(t, err)
	emptyBytes, err := record.CanonicalBytes(emptyContent)
	require.NoError(t, err)
	assert.NotEqual(t, nilBytes, emptyBytes)

	nilHash, err := record.ComputeHash(nilContent)
	require.NoError(t, err)
	emptyHash, err := record.ComputeHash(emptyContent)
	require.NoError(t, err)
	assert.NotEqual(t, nilHash, emptyHash)
}

// TestCanonicalBytesTimeZoneInsensitive verifies that two equal instants in
// different time zones encode identically, because At and RecordedAt are
// normalised to UTC before formatting.
func TestCanonicalBytesTimeZoneInsensitive(t *testing.T) {
	utc := populatedRecord(t)
	utc.At = mustParse(t, "2026-09-28T12:34:56.123456789Z")
	utc.RecordedAt = utc.At

	offset := populatedRecord(t)
	// Same instant, expressed with a +02:00 offset.
	offset.At = mustParse(t, "2026-09-28T14:34:56.123456789+02:00")
	offset.RecordedAt = offset.At

	ub, err := record.CanonicalBytes(utc)
	require.NoError(t, err)
	ob, err := record.CanonicalBytes(offset)
	require.NoError(t, err)
	assert.Equal(t, ub, ob)

	uh, err := record.ComputeHash(utc)
	require.NoError(t, err)
	oh, err := record.ComputeHash(offset)
	require.NoError(t, err)
	assert.Equal(t, uh, oh)
}

// TestCanonicalBytesReasonError verifies the fallible signature is honoured: a
// record carrying an invalid (zero) Reason cannot be canonicalised, because the
// reason encoding step fails. The error is propagated, never swallowed.
func TestCanonicalBytesReasonError(t *testing.T) {
	r := populatedRecord(t)
	r.Reason = record.Reason{}
	_, err := record.CanonicalBytes(r)
	require.Error(t, err)
	_, err = record.ComputeHash(r)
	require.Error(t, err)
}

// TestLinkOK verifies the trivial chain-link predicate: it reports whether
// cur.PrevHash equals prev.Hash, and nothing more.
func TestLinkOK(t *testing.T) {
	prev := populatedRecord(t)
	prev.Hash = record.Hash{0: 7}

	linked := populatedRecord(t)
	linked.PrevHash = prev.Hash
	assert.True(t, record.LinkOK(prev, linked))

	broken := populatedRecord(t)
	broken.PrevHash = record.Hash{0: 8}
	assert.False(t, record.LinkOK(prev, broken))
}

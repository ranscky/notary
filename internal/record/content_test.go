package record

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestContentHashGoldenBytes pins the EXACT hex digest ContentHash produces.
//
// This is the SINGLE pin for Notary's subject content hash. It lives in
// internal/record beside the scheme itself, so one test covers both sides: the
// interceptor writes these bytes onto every record's Subject.ContentHash, and
// the reconciler compares a listed memory's digest against an add's submitted
// text. The scheme and its pin were moved here together, out of internal/mem0
// (a thin REST client that should not import the audit model to name a return
// type). Before either move each side had its own copy of the scheme, so a
// drift on one side left the other's tests green while kept_by_content_match
// silently stopped matching. Now a drift on either side fails here.
//
// The expected values are the frozen wire contract: they are the bytes already
// written into signed ledgers, so they must never change without a migration.
// They are byte-for-byte the values this test asserted before the scheme moved
// out of internal/mem0. "hello world" is a single part; ["ab","c"] is
// multi-part; the empty-part cases pin that a zero-length part still
// contributes its length prefix.
func TestContentHashGoldenBytes(t *testing.T) {
	hexOf := func(parts ...string) string {
		h := ContentHash(parts...)
		return hex.EncodeToString(h[:])
	}

	tests := []struct {
		name  string
		parts []string
		want  string
	}{
		{
			name:  "single part",
			parts: []string{"hello world"},
			want:  "b6ea11b697ca779ee4e5794c51aab1cfa48faa94fb99fa8940eb5eea5b04d099",
		},
		{
			name:  "multi part",
			parts: []string{"ab", "c"},
			want:  "ba6f7cee0593771558bffc101cca4b3ce74eed214c4e5cdb645ebeb3ef4880bd",
		},
		{
			name:  "no parts",
			parts: nil,
			want:  "a9587aae31bb911617aaacc13d5f26b405d51073f4a0bace5b1318f905ed5fb3",
		},
		{
			name:  "one empty part",
			parts: []string{""},
			want:  "0f4f82ac8583b33383db907dfcd4ab97e705c0488fc294f2f1441e25e0e2f1b9",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hexOf(tt.parts...))
		})
	}

	// The boundary case that proves the length prefixing: each part is
	// length-prefixed individually, so a part boundary cannot collide with a
	// different split of the same bytes. ["ab","c"] and ["a","bc"] must differ,
	// or a later content match could be fooled by how the parts were split.
	assert.NotEqual(t, hexOf("ab", "c"), hexOf("a", "bc"),
		"length-prefixing must stop ['ab','c'] and ['a','bc'] from colliding")
	assert.Equal(t, "55d2b50c12c9d48e1c8d44f7703661c965fad115ad43737faf5b8b8651bfa9eb", hexOf("a", "bc"))

	// A digest is SHA-256 sized, so it can never be the all-zero value a record
	// Validate rejects. (Defensive, but it documents the shape.)
	assert.NotEqual(t, "0000000000000000000000000000000000000000000000000000000000000000", hexOf("hello world"))
}

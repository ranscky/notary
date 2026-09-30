package mem0

import (
	"crypto/sha256"
	"encoding/binary"

	"notary/internal/record"
)

// contentDomain domain-separates the subject content hash from every other use
// of SHA-256 in Notary, so a content digest can never collide with a digest
// computed for another purpose over the same bytes.
const contentDomain = "notary/content/v1"

// ContentHash returns Notary's subject content digest of one or more text
// parts: sha256(contentDomain ‖ (uint32be(len(part)) ‖ part)…).
//
// It lives in this leaf package because both of its consumers already depend on
// it -- internal/interceptor/library writes the digest onto every record's
// Subject.ContentHash, and internal/reconcile compares a listed memory's digest
// against an add's submitted text. Keeping the scheme here gives it a SINGLE
// definition. Two copies, even byte-identical ones, can drift, and a drift in
// the content hash silently stops kept_by_content_match ever matching — a
// failure that looks exactly like "nothing happened". One definition, pinned by
// golden bytes in content_test.go, is what makes drift impossible to miss on
// either side.
//
// Length-prefixing each part individually is what stops distinct part
// boundaries from colliding: ["ab","c"] and ["a","bc"] hash differently, so a
// content match cannot be fooled by how the parts happen to be joined for
// display. Note that this is NOT the hash of a record's Content.Text: that text
// is a plain "\n" join, which is ambiguous across boundaries. The two are
// intentionally different inputs to the same digest, so ambiguity in the
// displayed text cannot affect the content hash.
//
// An add is hashed as ContentHash(messages...) and a listed or surfaced memory
// as ContentHash(memory text), which is why a single-message add and the memory
// it produced agree. The value returned here is the Subject.ContentHash of
// every record the interceptor writes, so its bytes are part of signed records:
// the algorithm is a frozen contract and must not change without a migration.
func ContentHash(parts ...string) record.Hash {
	h := sha256.New()
	h.Write([]byte(contentDomain))
	var n [4]byte
	for _, p := range parts {
		binary.BigEndian.PutUint32(n[:], uint32(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	var out record.Hash
	copy(out[:], h.Sum(nil))
	return out
}

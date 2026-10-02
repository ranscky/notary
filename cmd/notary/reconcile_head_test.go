package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"notary/internal/record"
)

// TestAppendedBy is the table test for the report's central decision — the one
// place the CLI can LIE about the ledger by claiming it appended a record it did
// not. The concurrent-writer row is the reason the decision is not simply "did
// the chain head move": the store documents that a ledger file is written one
// writer at a time and serializes by SQLite (_busy_timeout exists to make those
// writers block rather than drop an append), and the interceptor library writes
// to the same ledger from the customer's process while a scheduled
// `notary reconcile` runs. A record landed by another writer between the two
// head reads would otherwise make a deduplicated claim print "appended".
func TestAppendedBy(t *testing.T) {
	const (
		ours   = record.RecordID("claim-1")
		theirs = record.RecordID("other-writer-claim")
	)

	for _, tc := range []struct {
		name      string
		id        record.RecordID
		before    record.Record
		hasBefore bool
		after     record.Record
		hasAfter  bool
		want      bool
	}{
		{
			name:     "the first record the ledger ever holds",
			id:       ours,
			after:    record.Record{ID: ours, Seq: 0},
			hasAfter: true,
			want:     true,
		},
		{
			name:      "appended on top of an earlier head",
			id:        ours,
			before:    record.Record{ID: theirs, Seq: 4},
			hasBefore: true,
			after:     record.Record{ID: ours, Seq: 5},
			hasAfter:  true,
			want:      true,
		},
		{
			name:      "deduplicated against the record that is already the head",
			id:        ours,
			before:    record.Record{ID: ours, Seq: 5},
			hasBefore: true,
			after:     record.Record{ID: ours, Seq: 5},
			hasAfter:  true,
			want:      false,
		},
		{
			name:      "deduplicated against an earlier record; the head is another",
			id:        ours,
			before:    record.Record{ID: theirs, Seq: 5},
			hasBefore: true,
			after:     record.Record{ID: theirs, Seq: 5},
			hasAfter:  true,
			want:      false,
		},
		{
			name:      "another writer appended after us",
			id:        ours,
			before:    record.Record{ID: theirs, Seq: 4},
			hasBefore: true,
			after:     record.Record{ID: theirs, Seq: 5},
			hasAfter:  true,
			want:      false,
		},
		{
			name: "no head after the append (unreachable, but must not claim)",
			id:   ours,
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want,
				appendedBy(tc.id, tc.before, tc.hasBefore, tc.after, tc.hasAfter))
		})
	}
}

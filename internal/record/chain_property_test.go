package record_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/record"
)

// baselineRecord builds a Record with every hashed field populated, so a test
// can mutate exactly one field and attribute any digest change to it.
func baselineRecord(t *testing.T) record.Record {
	t.Helper()
	ts := mustParse(t, "2026-09-28T10:00:00Z")
	return record.Record{
		ID:         "rec-baseline",
		Seq:        5,
		At:         ts,
		RecordedAt: ts.Add(time.Second),
		Event:      record.EventMemorySurfaced,
		Reason:     validReason(t),
		Subject: record.Subject{
			MemoryID: "mem-1",
			Scope: record.Scope{
				UserID:  "user-1",
				AgentID: "agent-1",
				AppID:   "app-1",
				RunID:   "run-1",
			},
			ContentHash: nonZeroHash(),
		},
		Content:        &record.Content{Text: "hello", Sensitive: false},
		IdempotencyKey: record.IdemKey("idem-1"),
		PrevHash:       record.Hash{0: 9},
		SignerKeyID:    "key-1",
	}
}

// differentReason builds a second, valid Reason of a different kind and tier
// (Reconstructed) through the Task 2 constructors, so a reason mutation swaps
// a genuinely different claim rather than perturbing unexported fields.
func differentReason(t *testing.T) record.Reason {
	t.Helper()
	ev, err := record.NewReconstructedEvidence(
		[]record.RecordID{"rec-a", "rec-b"}, "kept-by-content-match", "v1", 0.5,
	)
	require.NoError(t, err)
	r, err := record.NewReconstructedReason(record.ReasonKeptByContentMatch, ev)
	require.NoError(t, err)
	return r
}

// TestEveryFieldChangesHash is the core tamper-evidence property: for each
// field that participates in the digest, mutating it changes the hash. If any
// of these passed unchanged, that field would be silently unprotected.
func TestEveryFieldChangesHash(t *testing.T) {
	baseline := baselineRecord(t)
	baselineHash, err := record.ComputeHash(baseline)
	require.NoError(t, err)

	mutations := []struct {
		name   string
		mutate func(*record.Record)
	}{
		{"ID", func(r *record.Record) { r.ID = record.RecordID("rec-other") }},
		{"Seq", func(r *record.Record) { r.Seq = 6 }},
		{"At", func(r *record.Record) { r.At = r.At.Add(time.Nanosecond) }},
		{"RecordedAt", func(r *record.Record) { r.RecordedAt = r.RecordedAt.Add(time.Nanosecond) }},
		{"Event", func(r *record.Record) { r.Event = record.EventMemoryKept }},
		{"Reason", func(r *record.Record) { r.Reason = differentReason(t) }},
		{"Subject.MemoryID", func(r *record.Record) { r.Subject.MemoryID = "mem-2" }},
		{"Subject.Scope", func(r *record.Record) { r.Subject.Scope.UserID = "user-2" }},
		{"Subject.ContentHash", func(r *record.Record) { r.Subject.ContentHash = record.Hash{0: 2} }},
		{"Content.Text", func(r *record.Record) { r.Content.Text = "goodbye" }},
		{"Content.Sensitive", func(r *record.Record) { r.Content.Sensitive = true }},
		{"Content nil-ness", func(r *record.Record) { r.Content = nil }},
		{"IdempotencyKey", func(r *record.Record) { r.IdempotencyKey = record.IdemKey("idem-2") }},
		{"PrevHash", func(r *record.Record) { r.PrevHash = record.Hash{0: 10} }},
		{"SignerKeyID", func(r *record.Record) { r.SignerKeyID = "key-2" }},
	}

	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			mutated := baseline
			m.mutate(&mutated)
			got, err := record.ComputeHash(mutated)
			require.NoError(t, err)
			assert.NotEqual(t, baselineHash, got, "mutating %s must change the hash", m.name)
		})
	}
}

// TestComputeHashEmptyStoreGenesis verifies the empty-store case: a record with
// Seq == 0 and PrevHash == GenesisHash is the legitimate first record and must
// hash without error.
func TestComputeHashEmptyStoreGenesis(t *testing.T) {
	r := baselineRecord(t)
	r.Seq = 0
	r.PrevHash = record.GenesisHash

	h, err := record.ComputeHash(r)
	require.NoError(t, err)
	assert.NotEqual(t, record.Hash{}, h, "a real record's hash must not be all-zero")
}

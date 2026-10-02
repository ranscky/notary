package reconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/mem0"
	"notary/internal/record"
)

// provenMemories is the scope listing the fixtures resolve to: one memory in the
// scope the add belongs to.
func provenMemories() []mem0.Memory {
	return []mem0.Memory{{ID: "mem-1", Memory: "hello world", UserID: "u1"}}
}

// TestOnePassResolvesAnAddAndKeepsItsMemory is the regression test for the
// two-pass convergence defect found by the live probe against real Mem0.
//
// Reconcile folded the worklist ONCE, before its add-resolution stage ran, so a
// memory that stage discovered could not be in wl.known when the kept stage
// needed it. A fresh add therefore took TWO passes: pass 1 wrote add_resolved,
// pass 2 wrote memory_kept. Every pre-existing fixture hid this, because each
// already carried a memory_kept record, leaving the chain pre-satisfied.
//
// A single pass over a ledger holding one unresolved add must yield BOTH claims.
func TestOnePassResolvesAnAddAndKeepsItsMemory(t *testing.T) {
	req := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)
	rc := New(&fakeReader{records: []record.Record{req}}, passClient(t, provenMemories()...))

	got, err := rc.Reconcile(context.Background(), Window{})
	require.NoError(t, err)

	byEvent := map[record.EventType][]record.Record{}
	for _, r := range got {
		byEvent[r.Event] = append(byEvent[r.Event], r)
	}

	require.Len(t, byEvent[record.EventAddResolved], 1,
		"the unresolved add must be resolved")
	require.Len(t, byEvent[record.EventMemoryKept], 1,
		"the memory resolved during this pass must be certified kept in the SAME pass; "+
			"deferring it to a second pass is the defect")
	assert.Equal(t, record.ReasonStoredByMem0, byEvent[record.EventMemoryKept][0].Reason.Kind())
}

// TestSecondPassAddsNothingNew is the fixpoint assertion that makes the
// documented promise -- re-running against an unchanged store appends nothing --
// true rather than merely stated.
//
// ledger.Append deduplicates on the idempotency key, so the promise holds
// exactly when a re-run derives no id the previous pass had not derived.
// Asserting on ids rather than on slice length is the point: a pass that
// re-derives the same claims is a no-op at the ledger, and that is the case
// worth pinning.
func TestSecondPassAddsNothingNew(t *testing.T) {
	req := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)
	client := passClient(t, provenMemories()...)

	first, err := New(&fakeReader{records: []record.Record{req}}, client).
		Reconcile(context.Background(), Window{})
	require.NoError(t, err)
	require.NotEmpty(t, first, "an empty first pass would make this test vacuous")

	// What a re-run sees: the original ledger PLUS the first pass's output.
	rerun := append([]record.Record{req}, first...)
	second, err := New(&fakeReader{records: rerun}, client).
		Reconcile(context.Background(), Window{})
	require.NoError(t, err)

	derived := make(map[record.RecordID]bool, len(first))
	for _, r := range first {
		derived[r.ID] = true
	}
	for _, r := range second {
		assert.True(t, derived[r.ID],
			"a re-run derived %s, which the previous pass had not: the pass is not at its fixpoint, "+
				"so re-running would append a record after all", r.ID)
	}
}

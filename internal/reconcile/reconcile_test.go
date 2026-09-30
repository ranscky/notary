package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/mem0"
	"notary/internal/record"
)

// testHash returns a deterministic, non-zero content hash so a fixture record
// can be built without depending on any other package.
func testHash(tag byte) record.Hash {
	var h record.Hash
	for i := range h {
		h[i] = tag
	}
	return h
}

// mustAddRequested builds an add_requested record whose Observed evidence is
// the AddPayload carrying eventID, exactly as the interceptor writes it.
func mustAddRequested(t *testing.T, id, eventID string, at time.Time) record.Record {
	t.Helper()
	reason := observedReason(t, record.ReasonAddAcknowledged, mem0.AddPayload{EventID: eventID, Status: "PENDING"})
	return record.Record{
		ID:         record.RecordID(id),
		At:         at,
		RecordedAt: at,
		Event:      record.EventAddRequested,
		Reason:     reason,
		Subject:    record.Subject{Scope: record.Scope{UserID: "u1"}, ContentHash: testHash(0x11)},
	}
}

// mustAddResolved builds an add_resolved record whose Observed evidence is the
// EventStatusResponse that resolved eventID.
func mustAddResolved(t *testing.T, id, eventID string, at time.Time) record.Record {
	t.Helper()
	reason := observedReason(t, record.ReasonStoredByMem0, mem0.EventStatusResponse{ID: eventID, Status: "SUCCEEDED"})
	return record.Record{
		ID:         record.RecordID(id),
		At:         at,
		RecordedAt: at,
		Event:      record.EventAddResolved,
		Reason:     reason,
		Subject:    record.Subject{Scope: record.Scope{UserID: "u1"}, ContentHash: testHash(0x22)},
	}
}

// observedReason builds an Observed reason of the given kind over payload,
// marshalled to JSON exactly as the interceptor would.
func observedReason(t *testing.T, kind record.ReasonKind, payload any) record.Reason {
	t.Helper()
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, b)
	require.NoError(t, err)
	reason, err := record.NewObservedReason(kind, ev)
	require.NoError(t, err)
	return reason
}

// fakeReader is an in-memory Reader. It never touches the network or a store.
// It records the bounds it was called with so a test can prove the reconciler
// passes explicit, non-zero bounds (the store returns nothing when a bound is
// the zero time).
type fakeReader struct {
	records      []record.Record
	err          error
	lastFrom     time.Time
	lastTo       time.Time
	sawZeroBound bool
}

func (f *fakeReader) ListRecords(from, to time.Time) ([]record.Record, error) {
	f.lastFrom, f.lastTo = from, to
	if from.IsZero() || to.IsZero() {
		f.sawZeroBound = true
	}
	if f.err != nil {
		return nil, f.err
	}
	return append([]record.Record(nil), f.records...), nil
}

// fixedTime is a reference wall time used by the fixtures.
var fixedTime = time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Requirement 5: unresolved adds are matched on EVENT ID, not a heuristic.
// ---------------------------------------------------------------------------

func TestWorklistFindsUnresolvedAdds(t *testing.T) {
	req := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)

	wl, err := buildWorklist([]record.Record{req})
	require.NoError(t, err)
	require.Len(t, wl.unresolvedAdds, 1, "an add_requested with no add_resolved is unresolved")
	assert.Equal(t, record.RecordID("r-add-1"), wl.unresolvedAdds[0].ID)
}

func TestWorklistExcludesResolvedAdds(t *testing.T) {
	req := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)
	res := mustAddResolved(t, "r-res-1", "evt-1", fixedTime.Add(time.Minute))

	wl, err := buildWorklist([]record.Record{req, res})
	require.NoError(t, err)
	assert.Empty(t, wl.unresolvedAdds, "an add whose event id has an add_resolved must not be a work item")
}

func TestWorklistMatchesAddsByEventIDNotContent(t *testing.T) {
	// Two adds carry identical content, differing only in event id; only one is
	// resolved. A content- or timing-based heuristic would wrongly clear the
	// other, so the unresolved one must survive and the resolved one must not.
	resolved := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)
	unresolved := mustAddRequested(t, "r-add-2", "evt-2", fixedTime)
	res := mustAddResolved(t, "r-res-1", "evt-1", fixedTime.Add(time.Minute))

	wl, err := buildWorklist([]record.Record{resolved, unresolved, res})
	require.NoError(t, err)
	require.Len(t, wl.unresolvedAdds, 1)
	assert.Equal(t, record.RecordID("r-add-2"), wl.unresolvedAdds[0].ID)
}

// ---------------------------------------------------------------------------
// Requirement 3: Window.Since filters on At, never on RecordedAt.
// ---------------------------------------------------------------------------

func TestWindowSinceFiltersOnAt(t *testing.T) {
	since := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	// At is BEFORE Since, but the record was WRITTEN after Since. Filtering on
	// RecordedAt would wrongly admit it; filtering on At must exclude it,
	// because the work item (the Mem0 event) predates the window.
	oldEvent := mustAddRequested(t, "r-old", "evt-old", since.Add(-time.Hour))
	oldEvent.RecordedAt = since.Add(time.Hour)

	// At is after Since: included.
	newEvent := mustAddRequested(t, "r-new", "evt-new", since.Add(time.Hour))

	got := (Window{Since: since}).filter([]record.Record{oldEvent, newEvent})

	require.Len(t, got, 1, "exactly the in-window event must survive")
	assert.Equal(t, record.RecordID("r-new"), got[0].ID,
		"a record whose At precedes Since must be excluded even when RecordedAt follows it")
}

func TestWindowSinceZeroAdmitsEveryRecord(t *testing.T) {
	records := []record.Record{
		mustAddRequested(t, "r-1", "evt-1", fixedTime),
		mustAddRequested(t, "r-2", "evt-2", fixedTime.Add(time.Hour)),
	}
	got := (Window{}).filter(records)
	assert.Len(t, got, len(records), "a zero Since is no bound")
}

// ---------------------------------------------------------------------------
// Requirement 4: a zero Scope is no filter; a non-zero Scope restricts.
// ---------------------------------------------------------------------------

func TestWindowScopeFilter(t *testing.T) {
	u1 := mustAddRequested(t, "r-u1", "evt-u1", fixedTime)
	u2 := mustAddRequested(t, "r-u2", "evt-u2", fixedTime)
	u2.Subject.Scope = record.Scope{UserID: "u2"}
	records := []record.Record{u1, u2}

	// All-zero scope: no filter.
	assert.Len(t, (Window{}).filter(records), 2, "an all-zero scope is no filter")

	// Non-zero scope: restrict to matching records.
	got := (Window{Scope: record.Scope{UserID: "u1"}}).filter(records)
	require.Len(t, got, 1)
	assert.Equal(t, record.RecordID("r-u1"), got[0].ID)
}

// ---------------------------------------------------------------------------
// The fold must be handed real bounds, or the store returns nothing.
// ---------------------------------------------------------------------------

func TestReconcileReadsWholeLedgerWithNonZeroBounds(t *testing.T) {
	req := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)
	r := &fakeReader{records: []record.Record{req}}
	rc := New(r, nil)

	// Window.Since is zero (the default). The reconciler must still pass
	// explicit, non-zero bounds, because the store filters `at >= from AND
	// at <= to` literally and returns NOTHING when either bound is zero -- a
	// silent bug that would make every ledger look empty.
	_, err := rc.Reconcile(context.Background(), Window{})
	require.NoError(t, err)

	assert.False(t, r.sawZeroBound, "Reconcile passed a zero time bound; ledger.ListRecords would return nothing")
	assert.False(t, r.lastFrom.IsZero(), "from bound must be explicit")
	assert.False(t, r.lastTo.IsZero(), "to bound must be explicit")
	assert.False(t, r.lastFrom.After(r.lastTo), "from bound must not exceed to bound")
}

// ---------------------------------------------------------------------------
// Requirement 2: Reconcile returns records with a zero Seq and zero Hash.
// ---------------------------------------------------------------------------

func TestReconcileReturnedRecordsCarryNoChainPosition(t *testing.T) {
	req := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)
	rc := New(&fakeReader{records: []record.Record{req}}, nil)

	got, err := rc.Reconcile(context.Background(), Window{})
	require.NoError(t, err)
	for _, r := range got {
		assert.Zero(t, r.Seq, "reconciler must not assign a chain position; ledger.Append does")
		assert.Equal(t, record.Hash{}, r.Hash, "reconciler must not compute a digest; ledger.Append does")
	}
}

func TestCollectStageRejectsAssignedChainPosition(t *testing.T) {
	_, err := collectStage(nil, "resolveAdd", []record.Record{{ID: "r-1", Seq: 3}})
	require.Error(t, err, "a producer that assigns Seq must fail loudly, not be silently accepted")
	assert.Contains(t, err.Error(), "seq")
}

func TestCollectStageRejectsPrecomputedDigest(t *testing.T) {
	_, err := collectStage(nil, "resolveAdd", []record.Record{{ID: "r-1", Hash: testHash(0xAB)}})
	require.Error(t, err, "a producer that computes Hash must fail loudly")
	assert.Contains(t, err.Error(), "hash")
}

func TestCollectStageAppendsCleanRecords(t *testing.T) {
	got, err := collectStage(nil, "resolveAdd", []record.Record{{ID: "r-1"}, {ID: "r-2"}})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, record.RecordID("r-2"), got[1].ID)
}

// ---------------------------------------------------------------------------
// Failure behaviour: a reader error fails the pass loudly (§9.2).
// ---------------------------------------------------------------------------

func TestReconcilePropagatesReaderError(t *testing.T) {
	sentinel := errors.New("ledger unavailable")
	rc := New(&fakeReader{err: sentinel}, nil)

	_, err := rc.Reconcile(context.Background(), Window{})
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel, "the reader's error must be wrapped, not swallowed")
}

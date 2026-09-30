package reconcile

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/mem0"
	"notary/internal/record"
)

// ---------------------------------------------------------------------------
// Fixtures: one Mem0 server that answers BOTH the scope enumeration
// (POST /v3/memories/) and per-memory history (GET /v1/memories/{id}/history/),
// because resolveRemoved reads both. It never touches the network.
// ---------------------------------------------------------------------------

// removedFetcher records which memories' history the server was asked for, so a
// test can prove a still-listed memory never triggers a History call.
type removedFetcher struct {
	mu           sync.Mutex
	historyCalls []string
}

func (f *removedFetcher) record(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.historyCalls = append(f.historyCalls, id)
}

func (f *removedFetcher) historyCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.historyCalls)
}

func (f *removedFetcher) sawHistory(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.historyCalls {
		if c == id {
			return true
		}
	}
	return false
}

// removedClient starts an httptest server serving the scope enumeration and the
// per-memory history. page is keyed on the filters' user_id (so a test can
// serve different scopes differently) and the page number; history returns a
// response (or a non-200 status) for a memory id.
func removedClient(
	t *testing.T,
	page func(userID string, pageNum int) enumPage,
	history func(memoryID string) (mem0.HistoryResponse, int),
) (*mem0.Client, *removedFetcher) {
	t.Helper()
	f := &removedFetcher{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v3/memories/":
			var body struct {
				Filters struct {
					UserID string `json:"user_id"`
				} `json:"filters"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			pageNum, _ := strconv.Atoi(r.URL.Query().Get("page"))
			writeRemovedJSON(w, page(body.Filters.UserID, pageNum))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/memories/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/memories/"), "/history/")
			f.record(id)
			resp, status := history(id)
			if status != http.StatusOK {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"mem0 outage"}`))
				return
			}
			writeRemovedJSON(w, resp)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return mem0.NewClient(srv.URL, "test-key", nil), f
}

func writeRemovedJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// removedPage is a complete single page listing exactly mems.
func removedPage(mems ...mem0.Memory) func(string, int) enumPage {
	return func(string, int) enumPage { return enumPage{Count: len(mems), Results: mems} }
}

// historyDelete is a history handler returning a single DELETE entry whose
// corroborating timestamps are created/updated. Passing an empty created falls
// back to updated.
func historyDelete(created, updated string) func(string) (mem0.HistoryResponse, int) {
	return func(memoryID string) (mem0.HistoryResponse, int) {
		return mem0.HistoryResponse{{
			ID:        "hist-1",
			MemoryID:  memoryID,
			OldMemory: strPtr("hello world"),
			NewMemory: nil,
			Event:     "DELETE",
			CreatedAt: created,
			UpdatedAt: updated,
		}}, http.StatusOK
	}
}

// mustEnum builds a valid CompleteEnumeration through the only constructor that
// can, so a test never hand-writes one.
func mustEnum(t *testing.T, c *mem0.Client, scope record.Scope) mem0.CompleteEnumeration {
	t.Helper()
	e, err := c.GetAllComplete(context.Background(), mem0.GetAllRequest{Filters: scopeFilters(scope)})
	require.NoError(t, err)
	require.True(t, e.Valid())
	return e
}

// removedScope is the single entity scope the removal fixtures live in.
var removedScope = record.Scope{UserID: "u1"}

// removedKnown is a memory the ledger establishes exists.
func removedKnown(memoryID string) knownMemory {
	return knownMemory{
		MemoryID:    memoryID,
		Scope:       removedScope,
		ContentHash: record.ContentHash("hello world"),
		Basis:       record.RecordID("memory_kept:stored_by_mem0:" + memoryID),
		At:          fixedTime,
	}
}

// ledgerKept is the ledger record that establishes a memory exists, so a
// Reconcile-level test can drive the fold into a known memory.
func ledgerKept(t *testing.T, memoryID string, scope record.Scope) record.Record {
	t.Helper()
	return record.Record{
		ID:         record.RecordID("memory_kept:stored_by_mem0:" + memoryID),
		At:         fixedTime,
		RecordedAt: fixedTime,
		Event:      record.EventMemoryKept,
		Reason:     observedReason(t, record.ReasonStoredByMem0, mem0.Memory{ID: memoryID, Memory: "hello world", UserID: scope.UserID}),
		Subject:    record.Subject{MemoryID: memoryID, Scope: scope, ContentHash: record.ContentHash("hello world")},
	}
}

// internalNote decodes the opaque text an Internal reason carries. The record
// package exposes no accessor for it, so it is read from the encoded bytes,
// exactly as the reconstructed evidence is asserted elsewhere.
func internalNote(t *testing.T, r record.Record) string {
	t.Helper()
	b, err := r.Reason.Encode()
	require.NoError(t, err)
	var wire struct {
		Internal struct {
			Opaque string `json:"opaque"`
		} `json:"internal"`
	}
	require.NoError(t, json.Unmarshal(b, &wire))
	return wire.Internal.Opaque
}

// ---------------------------------------------------------------------------
// Spec §5 row 7: memory_dropped + removed_by_mem0 (Internal) only when a known
// memory is absent from a COMPLETE enumeration AND its history corroborates a
// removal.
// ---------------------------------------------------------------------------

// TestRemovedInternalWhenHistoryShowsDelete is the positive case: mem-1 is a
// known memory, absent from a complete enumeration of its scope, and its Mem0
// history records a DELETE. The claim is Internal and the note is an opacity
// marker.
func TestRemovedInternalWhenHistoryShowsDelete(t *testing.T) {
	known := removedKnown("mem-1")
	client, _ := removedClient(t,
		removedPage(), // complete, empty: mem-1 is absent
		historyDelete("2024-01-01T12:00:00Z", "2024-01-01T12:05:00Z"),
	)
	rc := New(&fakeReader{}, client)

	got, err := rc.resolveRemoved(context.Background(), mustEnum(t, client, removedScope), removedScope, []knownMemory{known})
	require.NoError(t, err)
	require.Len(t, got, 1, "a known memory absent from a complete enumeration and corroborated by history warrants one memory_dropped claim")

	rec := got[0]
	require.NoError(t, rec.Validate(), "the produced record must be a valid claim the caller can append")

	assert.Equal(t, record.EventMemoryDropped, rec.Event)
	assert.Equal(t, record.ReasonRemovedByMem0, rec.Reason.Kind())
	assert.Equal(t, record.Internal, rec.Reason.Tier(),
		"the EVENT is witnessed (the memory is absent) but the REASON is not: Mem0 exposed none, so the claim is Internal")
	assert.Equal(t, record.RecordID("memory_dropped:removed_by_mem0:mem-1"), rec.ID,
		"the record id is qualified by the reason kind, as records.id is UNIQUE while duplicate suppression matches only the idempotency key")
	assert.Equal(t, "mem-1", rec.Subject.MemoryID)
	assert.Equal(t, removedScope, rec.Subject.Scope)
	assert.Equal(t, known.ContentHash, rec.Subject.ContentHash)

	// The rule name is recorded as the reason kind; the rule version rides in
	// the idempotency key. Both must match the registry.
	rule, ok := LookupRule(RuleRemovedByMem0)
	require.True(t, ok, "the rule must be in the registry")
	assert.Equal(t, "removed_by_mem0", rule.Name)
	assert.Equal(t, "1", rule.Version, "the rule is v1")
	assert.Equal(t, rule.Kind, rec.Reason.Kind(), "the recorded kind names the rule")

	// At is the corroborating history entry's timestamp, parsed from Mem0. When
	// CreatedAt and UpdatedAt disagree, CreatedAt wins (see the producer).
	assert.Equal(t, time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), rec.At,
		"At is the history entry's event time, not Mem0's UpdatedAt and not the zero time")
	assert.True(t, rec.RecordedAt.IsZero(), "RecordedAt is left zero for ledger.Append to stamp")

	// The note is a non-empty opacity marker.
	note := internalNote(t, rec)
	assert.NotEmpty(t, note)
	assert.Contains(t, note, "no reason", "the note must state that Mem0 exposed no reason")

	// The claim identity is per memory, per rule: a second identical call
	// derives the identical key so ledger.Append suppresses the duplicate.
	require.NotEmpty(t, rec.IdempotencyKey)
	again, err := rc.resolveRemoved(context.Background(), mustEnum(t, client, removedScope), removedScope, []knownMemory{known})
	require.NoError(t, err)
	require.Len(t, again, 1)
	assert.Equal(t, rec.IdempotencyKey, again[0].IdempotencyKey,
		"a second identical call must re-derive the identical key")
}

// TestRemovedInternalWhenHistoryShowsUpdate pins the second corroborating
// event kind: an UPDATE entry is also evidence Mem0 removed/superseded a memory
// the enumeration no longer lists.
func TestRemovedInternalWhenHistoryShowsUpdate(t *testing.T) {
	known := removedKnown("mem-1")
	client, _ := removedClient(t, removedPage(), func(memoryID string) (mem0.HistoryResponse, int) {
		return mem0.HistoryResponse{{
			ID:        "hist-1",
			MemoryID:  memoryID,
			OldMemory: strPtr("old text"),
			NewMemory: strPtr("new text"),
			Event:     "UPDATE",
			CreatedAt: "2024-01-01T12:00:00Z",
			UpdatedAt: "2024-01-01T12:00:00Z",
		}}, http.StatusOK
	})
	rc := New(&fakeReader{}, client)

	got, err := rc.resolveRemoved(context.Background(), mustEnum(t, client, removedScope), removedScope, []knownMemory{known})
	require.NoError(t, err)
	require.Len(t, got, 1, "an UPDATE entry corroborates a removal just as a DELETE does")
	assert.Equal(t, record.Internal, got[0].Reason.Tier())
}

// TestRemovedNotClaimedWhenMemoryIsStillListed is the load-bearing negative: a
// memory PRESENT in the complete enumeration is not removed, so no claim may be
// written -- and its history must not even be consulted.
func TestRemovedNotClaimedWhenMemoryIsStillListed(t *testing.T) {
	known := removedKnown("mem-1")
	client, fetcher := removedClient(t,
		removedPage(mem0.Memory{ID: "mem-1", Memory: "hello world", UserID: "u1"}),
		historyDelete("2024-01-01T12:00:00Z", "2024-01-01T12:00:00Z"),
	)
	rc := New(&fakeReader{}, client)

	got, err := rc.resolveRemoved(context.Background(), mustEnum(t, client, removedScope), removedScope, []knownMemory{known})
	require.NoError(t, err)
	assert.Empty(t, got, "a memory still present in the complete enumeration must not be claimed removed")
	assert.Zero(t, fetcher.historyCallCount(),
		"a still-listed memory short-circuits before any History call")
}

// TestRemovedNotClaimedWhenHistoryShowsOnlyAdd pins that an ADD-only history
// does not corroborate a removal: absence from a listing alone is not enough,
// so a memory that was added but never deleted or updated yields no claim.
func TestRemovedNotClaimedWhenHistoryShowsOnlyAdd(t *testing.T) {
	known := removedKnown("mem-1")
	client, _ := removedClient(t, removedPage(), func(memoryID string) (mem0.HistoryResponse, int) {
		return mem0.HistoryResponse{{
			ID:        "hist-1",
			MemoryID:  memoryID,
			OldMemory: nil,
			NewMemory: strPtr("hello world"),
			Event:     "ADD",
			CreatedAt: "2024-01-01T12:00:00Z",
			UpdatedAt: "2024-01-01T12:00:00Z",
		}}, http.StatusOK
	})
	rc := New(&fakeReader{}, client)

	got, err := rc.resolveRemoved(context.Background(), mustEnum(t, client, removedScope), removedScope, []knownMemory{known})
	require.NoError(t, err)
	assert.Empty(t, got, "an ADD entry is not a removal: absence from a listing alone must not become a claim")
}

// TestRemovedNotClaimedFromAnIncompleteEnumeration pins requirement 1 directly:
// resolveRemoved takes a mem0.CompleteEnumeration, and the zero value is
// deliberately invalid, so holding one is an error to report -- never an
// absence to read.
func TestRemovedNotClaimedFromAnIncompleteEnumeration(t *testing.T) {
	rc := New(&fakeReader{}, nil)
	got, err := rc.resolveRemoved(context.Background(), mem0.CompleteEnumeration{}, removedScope, []knownMemory{removedKnown("mem-1")})
	require.Error(t, err, "an invalid enumeration is a contract violation to report, not a listing to read absence from")
	assert.Empty(t, got)
}

// TestReconcileRemovedFailsOnAnIncompleteEnumeration is the end-to-end companion:
// a get_all walk that contradicts its own count cannot certify the scope, so the
// whole pass fails loudly and writes nothing.
func TestReconcileRemovedFailsOnAnIncompleteEnumeration(t *testing.T) {
	// First page reports count 1 but serves no results: the walk collects 0 of a
	// reported 1, so GetAllComplete fails with ErrEnumerationIncomplete.
	client, _ := removedClient(t, func(string, int) enumPage {
		return enumPage{Count: 1, Results: nil}
	}, historyDelete("2024-01-01T12:00:00Z", "2024-01-01T12:00:00Z"))
	r := &fakeReader{records: []record.Record{ledgerKept(t, "mem-1", removedScope)}}
	rc := New(r, client)

	got, err := rc.Reconcile(context.Background(), Window{})
	require.Error(t, err, "an enumeration that disagrees with its own count must fail the pass loudly")
	assert.ErrorIs(t, err, mem0.ErrEnumerationIncomplete)
	assert.Empty(t, got, "no claim may be derived from an incomplete enumeration")
}

// TestRemovedPrefersCreatedAtAndFallsBackToUpdatedAt pins the exact history
// timestamp field: the entry's CreatedAt is the event time (Mem0's UpdatedAt is
// not, by Task 6's settled reasoning), and UpdatedAt is consulted only when
// CreatedAt is absent.
func TestRemovedPrefersCreatedAtAndFallsBackToUpdatedAt(t *testing.T) {
	t.Run("CreatedAt wins when present", func(t *testing.T) {
		known := removedKnown("mem-1")
		client, _ := removedClient(t, removedPage(),
			historyDelete("2024-01-01T12:00:00Z", "2024-01-01T12:05:00Z"))
		rc := New(&fakeReader{}, client)

		got, err := rc.resolveRemoved(context.Background(), mustEnum(t, client, removedScope), removedScope, []knownMemory{known})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), got[0].At,
			"At is the entry's CreatedAt, the event time")
	})

	t.Run("UpdatedAt is the fallback when CreatedAt is absent", func(t *testing.T) {
		known := removedKnown("mem-1")
		client, _ := removedClient(t, removedPage(),
			historyDelete("", "2024-01-01T12:05:00Z"))
		rc := New(&fakeReader{}, client)

		got, err := rc.resolveRemoved(context.Background(), mustEnum(t, client, removedScope), removedScope, []knownMemory{known})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, time.Date(2024, 1, 1, 12, 5, 0, 0, time.UTC), got[0].At)
	})
}

// TestRemovedFailsLoudlyOnAnUnparseableHistoryTimestamp pins the At failure
// behaviour: an unparseable corroborating timestamp is a loud error, never a
// zero or a guessed At (a zero-dated claim would be invisible to any windowed
// read -- silently wrong).
func TestRemovedFailsLoudlyOnAnUnparseableHistoryTimestamp(t *testing.T) {
	known := removedKnown("mem-1")
	client, _ := removedClient(t, removedPage(),
		historyDelete("not-a-timestamp", "2024-01-01T12:05:00Z"))
	rc := New(&fakeReader{}, client)

	got, err := rc.resolveRemoved(context.Background(), mustEnum(t, client, removedScope), removedScope, []knownMemory{known})
	require.Error(t, err, "an unparseable history timestamp must fail loudly")
	assert.Empty(t, got, "no claim may be written with an unparseable At")

	// An entry with no timestamp at all is equally a loud failure.
	client2, _ := removedClient(t, removedPage(), historyDelete("", ""))
	rc2 := New(&fakeReader{}, client2)
	got2, err2 := rc2.resolveRemoved(context.Background(), mustEnum(t, client2, removedScope), removedScope, []knownMemory{known})
	require.Error(t, err2, "a corroborating entry with no timestamp must fail loudly")
	assert.Empty(t, got2)
}

// TestRemovedPropagatesHistoryError pins that a History outage fails the pass
// loudly and is never read as "no removal" (which would be a silent no-op).
func TestRemovedPropagatesHistoryError(t *testing.T) {
	known := removedKnown("mem-1")
	client, _ := removedClient(t, removedPage(), func(string) (mem0.HistoryResponse, int) {
		return nil, http.StatusInternalServerError
	})
	rc := New(&fakeReader{}, client)

	got, err := rc.resolveRemoved(context.Background(), mustEnum(t, client, removedScope), removedScope, []knownMemory{known})
	require.Error(t, err, "a History error must fail the pass, not be mistaken for \"no removal\"")
	var httpErr *mem0.HTTPError
	require.ErrorAs(t, err, &httpErr, "the underlying Mem0 error must survive wrapping")
	assert.Empty(t, got)
}

// TestRemovedFailsLoudlyWithoutAClient mirrors resolveKept and resolveAdd: a nil
// Mem0 client is a misconfigured reconciler, not "nothing to claim".
func TestRemovedFailsLoudlyWithoutAClient(t *testing.T) {
	// Build a valid enumeration with a working client, then hand it to a
	// reconciler that holds none: the corroborating History call cannot be made.
	client, _ := removedClient(t, removedPage(), historyDelete("2024-01-01T12:00:00Z", "2024-01-01T12:00:00Z"))
	enum := mustEnum(t, client, removedScope)

	rc := New(&fakeReader{}, nil)
	got, err := rc.resolveRemoved(context.Background(), enum, removedScope, []knownMemory{removedKnown("mem-1")})
	require.Error(t, err, "a nil client is a misconfigured reconciler, not a clean pass")
	assert.ErrorIs(t, err, ErrNoMem0Client)
	assert.Empty(t, got)
}

// ---------------------------------------------------------------------------
// The opacity marker: the note must not invent a cause.
// ---------------------------------------------------------------------------

// TestRemovedRecordsNoGuessAtWhy is the whole point of the Internal tier: the
// event is witnessed, the reason is not, so the note states only that Mem0
// removed the memory and exposed no reason -- and none of the causes Notary
// must never guess.
func TestRemovedRecordsNoGuessAtWhy(t *testing.T) {
	known := removedKnown("mem-1")
	client, _ := removedClient(t, removedPage(),
		historyDelete("2024-01-01T12:00:00Z", "2024-01-01T12:00:00Z"))
	rc := New(&fakeReader{}, client)

	got, err := rc.resolveRemoved(context.Background(), mustEnum(t, client, removedScope), removedScope, []knownMemory{known})
	require.NoError(t, err)
	require.Len(t, got, 1)

	note := internalNote(t, got[0])
	require.NotEmpty(t, note, "the note is the opacity marker and must never be empty")

	lower := strings.ToLower(note)
	for _, forbidden := range []string{
		"expired", "evicted", "pruned", "duplicate", "threshold", "superseded", "decayed",
	} {
		assert.NotContains(t, lower, forbidden,
			"the note must not invent a cause: %q is a guess Notary may not make", forbidden)
	}
	assert.Contains(t, lower, "no reason",
		"the note must say that Mem0 exposed no reason")
}

// ---------------------------------------------------------------------------
// End-to-end: the removal stage is scoped to the scope it enumerated.
// ---------------------------------------------------------------------------

// TestReconcileRemovedIsScopedToTheEnumeratedScope proves the wiring never
// crosses scopes: mem-a is absent from u1's enumeration and its history shows a
// DELETE, while mem-b is still listed in u2, so only mem-a yields a claim --
// and mem-b's history is never consulted.
func TestReconcileRemovedIsScopedToTheEnumeratedScope(t *testing.T) {
	u1 := record.Scope{UserID: "u1"}
	u2 := record.Scope{UserID: "u2"}

	client, fetcher := removedClient(t,
		func(userID string, _ int) enumPage {
			switch userID {
			case "u1":
				return enumPage{Count: 0, Results: nil} // mem-a absent
			case "u2":
				return enumPage{Count: 1, Results: []mem0.Memory{{ID: "mem-b", Memory: "hello world", UserID: "u2"}}}
			default:
				return enumPage{}
			}
		},
		historyDelete("2024-01-01T12:00:00Z", "2024-01-01T12:00:00Z"),
	)
	r := &fakeReader{records: []record.Record{
		ledgerKept(t, "mem-a", u1),
		ledgerKept(t, "mem-b", u2),
	}}
	rc := New(r, client)

	got, err := rc.Reconcile(context.Background(), Window{})
	require.NoError(t, err)

	var dropped []record.Record
	for _, rec := range got {
		if rec.Event == record.EventMemoryDropped {
			dropped = append(dropped, rec)
		}
	}
	require.Len(t, dropped, 1, "only the memory absent from its OWN scope's enumeration may be claimed removed")
	assert.Equal(t, record.RecordID("memory_dropped:removed_by_mem0:mem-a"), dropped[0].ID)
	assert.Equal(t, u1, dropped[0].Subject.Scope)
	assert.True(t, fetcher.sawHistory("mem-a"), "mem-a's history corroborates its removal")
	assert.False(t, fetcher.sawHistory("mem-b"),
		"a still-listed memory in another scope must never have its history consulted for a removal")
}

// ---------------------------------------------------------------------------
// Finding 3: resolveRemoved's scope contract is a structural guard.
// ---------------------------------------------------------------------------

// TestRemovedRejectsKnownMemoryFromAnotherScope pins the guard: a
// CompleteEnumeration carries no scope, so resolveRemoved is handed the scope it
// enumerates explicitly and must REJECT any known memory from a different scope
// rather than compare it against the listing. Comparing a memory in one scope
// against another scope's enumeration would silently mis-attribute a removal --
// so the convention is enforced, not merely documented.
func TestRemovedRejectsKnownMemoryFromAnotherScope(t *testing.T) {
	// A valid, complete enumeration of removedScope (u1). It is empty, so a
	// memory IN u1 would be claimed removed -- which makes the guard's effect
	// (no claim, an error) meaningful.
	client, _ := removedClient(t, removedPage(),
		historyDelete("2024-01-01T12:00:00Z", "2024-01-01T12:00:00Z"))
	enum := mustEnum(t, client, removedScope)

	otherScope := record.Scope{UserID: "u2"}
	otherKnown := knownMemory{
		MemoryID:    "mem-other",
		Scope:       otherScope,
		ContentHash: record.ContentHash("hello world"),
		Basis:       record.RecordID("memory_kept:stored_by_mem0:mem-other"),
		At:          fixedTime,
	}
	rc := New(&fakeReader{}, client)

	got, err := rc.resolveRemoved(context.Background(), enum, removedScope, []knownMemory{otherKnown})
	require.Error(t, err, "a known memory from a different scope must be rejected, not compared against this enumeration")
	assert.ErrorIs(t, err, ErrScopeMismatch, "the rejection must be the matchable ErrScopeMismatch")
	assert.Empty(t, got, "no claim may be derived for a memory outside the enumerated scope")

	// The SAME memory, handed with its OWN scope, is a legitimate subject (an
	// absent memory whose history shows a DELETE): the guard fires on the scope
	// mismatch, not on the memory.
	got2, err2 := rc.resolveRemoved(context.Background(),
		mustEnum(t, client, otherScope), otherScope, []knownMemory{otherKnown})
	require.NoError(t, err2)
	require.Len(t, got2, 1,
		"with its own scope the memory is a legitimate removal candidate, proving the guard rejects the mismatch, not the memory")
}

// ---------------------------------------------------------------------------
// Finding 4: a parseable-but-ZERO history timestamp is rejected.
// ---------------------------------------------------------------------------

// TestRemovedFailsLoudlyOnAZeroHistoryTimestamp pins the IsZero guard: Mem0 can
// return a parseable-but-zero timestamp (0001-01-01T00:00:00Z), which
// time.Parse accepts but which would date a signed claim at year 1 and hide it
// from every windowed read -- the exact silent-wrong historyEntryTime warns
// against. It must fail as loudly as an unparseable timestamp.
func TestRemovedFailsLoudlyOnAZeroHistoryTimestamp(t *testing.T) {
	known := removedKnown("mem-1")

	// CreatedAt is the literal zero time.
	t.Run("zero CreatedAt", func(t *testing.T) {
		client, _ := removedClient(t, removedPage(),
			historyDelete("0001-01-01T00:00:00Z", "2024-01-01T12:05:00Z"))
		rc := New(&fakeReader{}, client)

		got, err := rc.resolveRemoved(context.Background(), mustEnum(t, client, removedScope), removedScope, []knownMemory{known})
		require.Error(t, err, "a zero CreatedAt must fail loudly, never date a claim at year 1")
		assert.Empty(t, got, "no claim may be written with a zero At")
	})

	// The UpdatedAt fallback is guarded too: a zero UpdatedAt is equally a loud
	// failure.
	t.Run("zero UpdatedAt fallback", func(t *testing.T) {
		client, _ := removedClient(t, removedPage(),
			historyDelete("", "0001-01-01T00:00:00Z"))
		rc := New(&fakeReader{}, client)

		got, err := rc.resolveRemoved(context.Background(), mustEnum(t, client, removedScope), removedScope, []knownMemory{known})
		require.Error(t, err, "a zero UpdatedAt fallback must also fail loudly")
		assert.Empty(t, got)
	})
}

// ---------------------------------------------------------------------------
// Finding 2: each scope is enumerated exactly ONCE per pass.
// ---------------------------------------------------------------------------

// TestReconcileEnumeratesEachScopeOncePerPass pins that resolveKept and
// resolveRemoved SHARE one enumeration per scope. They used to each call
// GetAllComplete -- a paginated Mem0 walk up to a 1000-page bound -- doubling
// network and rate-limit cost per scope and, worse, letting a write that landed
// between the two walks yield BOTH a stored_by_mem0 and a removed_by_mem0 claim
// about the same memory in one pass. This counts the get_all requests: exactly
// one per pass for a scope that both producers consider.
func TestReconcileEnumeratesEachScopeOncePerPass(t *testing.T) {
	scope := record.Scope{UserID: "u1"}

	var mu sync.Mutex
	enumCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v3/memories/":
			mu.Lock()
			enumCalls++
			mu.Unlock()
			// A complete, EMPTY listing: resolveKept claims nothing, and
			// resolveRemoved consults mem-1's history for a possible removal.
			writeRemovedJSON(w, enumPage{Count: 0, Results: nil})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/memories/"):
			// ADD-only history: absence from the listing alone does not
			// corroborate a removal, so resolveRemoved claims nothing.
			writeRemovedJSON(w, mem0.HistoryResponse{{
				ID:        "hist-1",
				MemoryID:  "mem-1",
				NewMemory: strPtr("hello world"),
				Event:     "ADD",
				CreatedAt: "2024-01-01T12:00:00Z",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := mem0.NewClient(srv.URL, "test-key", nil)

	r := &fakeReader{records: []record.Record{ledgerKept(t, "mem-1", scope)}}
	rc := New(r, client)

	_, err := rc.Reconcile(context.Background(), Window{})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, enumCalls,
		"each scope must be enumerated exactly ONCE per pass, shared by resolveKept and resolveRemoved")
}

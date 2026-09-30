package reconcile

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/ledger"
	"notary/internal/mem0"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// ---------------------------------------------------------------------------
// Fixtures: a real, signed SQLite ledger plus a fake Mem0 server, so the
// integrity properties are proven over genuinely appended state rather than a
// fake reader. No test touches the network (httptest only).
// ---------------------------------------------------------------------------

// integrityKeySeed is a fixed 32-byte ed25519 seed, so the ledger these tests
// sign is deterministic across runs. It is test material only and must never
// appear in production.
var integrityKeySeed = []byte("0123456789abcdef0123456789abcdef")

// integrityKeyEnv names the environment variable the test holds the base64 seed
// in. NewSigner never invents a key, so it must be set before NewSigner runs.
const integrityKeyEnv = "NOTARY_RECONCILE_INTEGRITY_KEY"

// newIntegrityLedger returns a signing ledger over a fresh SQLite store in a
// temp dir, plus the store so a test can read chain position and row count. The
// store is closed at test cleanup.
func newIntegrityLedger(t *testing.T) (*store.SQLiteStore, *ledger.Ledger) {
	t.Helper()
	t.Setenv(integrityKeyEnv, base64.StdEncoding.EncodeToString(integrityKeySeed))
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: integrityKeyEnv})
	require.NoError(t, err)

	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	return st, ledger.New(st, sg, nil)
}

// appendAll appends every record through the ledger, so the seeds are genuinely
// chained and signed exactly as production writes them.
func appendAll(t *testing.T, l *ledger.Ledger, recs ...record.Record) {
	t.Helper()
	for _, rec := range recs {
		_, err := l.Append(rec)
		require.NoError(t, err)
	}
}

// rowCount returns the number of stored rows -- the ledger's length -- so a test
// can prove a pass appended nothing.
func rowCount(t *testing.T, st *store.SQLiteStore) int {
	t.Helper()
	entries, err := st.SeqEntries()
	require.NoError(t, err)
	return len(entries)
}

// claimIDs returns the record ids of derived claims, for set comparison.
func claimIDs(recs []record.Record) []record.RecordID {
	ids := make([]record.RecordID, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	return ids
}

// addResolvedSeed builds an add_resolved record that ESTABLISHES a known memory:
// a stored_by_mem0 outcome carrying the produced memory id on the Subject, which
// the fold reads as "Notary knows memoryID exists in scope". It is how a ledger
// legitimately acquires the subject the later producers derive claims about.
func addResolvedSeed(t *testing.T, id, eventID, memoryID string, scope record.Scope, at time.Time) record.Record {
	t.Helper()
	status := mem0.EventStatusResponse{
		ID:      eventID,
		Status:  "SUCCEEDED",
		Results: []mem0.EventResult{{ID: memoryID}},
	}
	return record.Record{
		ID:         record.RecordID(id),
		At:         at,
		RecordedAt: at,
		Event:      record.EventAddResolved,
		Reason:     observedReason(t, record.ReasonStoredByMem0, status),
		Subject: record.Subject{
			MemoryID:    memoryID,
			Scope:       scope,
			ContentHash: record.ContentHash("hello world"),
		},
	}
}

// seedKnownMemoryAndCoveringSearch appends the seed records that give the
// producers something to derive on EVERY pass, over scope {u1}: an add_resolved
// that ESTABLISHES mem-1, and a later covering search of that scope. The
// complete listing (served by the caller) contains mem-1, so resolveKept derives
// an Observed memory_kept, and the covering search makes resolveAbsent derive
// absent_from_search -- two claims, both re-derived on a second pass.
func seedKnownMemoryAndCoveringSearch(t *testing.T, l *ledger.Ledger) {
	t.Helper()
	scope := record.Scope{UserID: "u1"}
	appendAll(t, l,
		addResolvedSeed(t, "add_resolved:stored_by_mem0:evt-1", "evt-1", "mem-1", scope, fixedTime),
		searchRecord(t, "search-1", scope,
			mem0.SearchPerformedPayload{Query: "q", TopK: 5, Count: 0}, fixedTime.Add(time.Minute)),
	)
}

// ---------------------------------------------------------------------------
// The headline property: a second pass over an unchanged store appends ZERO
// records. This is what makes the command safe to schedule.
// ---------------------------------------------------------------------------

// TestSecondPassAppendsZeroRecords is the property the whole phase rests on:
// after a first pass derives and appends its claims, a second pass over the SAME
// store and ledger must re-derive the identical claims and append nothing,
// because every derived idempotency key already exists.
//
// It proves BOTH halves, so it cannot pass by producing nothing:
//
//   - the second pass DERIVES the same claims the first did (non-empty, equal
//     id sets), so the no-append result is meaningful; and
//   - every key the second pass derived is already stored, and appending each
//     second-pass claim returns the EXISTING record, leaving the chain's Seq,
//     head hash and row count unchanged.
//
// If DeriveClaimIdemKey ever incorporated the run (a clock, a pass counter, a
// cursor), the second pass would derive fresh keys and append duplicates; this
// test is what would catch that silently dangerous regression.
func TestSecondPassAppendsZeroRecords(t *testing.T) {
	st, l := newIntegrityLedger(t)
	seedKnownMemoryAndCoveringSearch(t, l)

	// The complete listing contains mem-1, so resolveKept derives an Observed
	// memory_kept in addition to the absent_from_search claim.
	client := enumerationClient(t, completePage(mem0.Memory{ID: "mem-1", Memory: "hello world", UserID: "u1"}))
	rc := New(l, client)
	ctx := context.Background()

	// PASS 1: derive and append everything.
	first, err := rc.Reconcile(ctx, Window{})
	require.NoError(t, err)
	require.NotEmpty(t, first, "the fixture must derive at least one claim, or this test is vacuous")

	// The first pass's keys must NOT already exist, or the append below would
	// prove nothing about pass 1 having written new rows.
	for _, rec := range first {
		_, present, err := st.ByIdemKey(rec.IdempotencyKey)
		require.NoError(t, err)
		assert.False(t, present, "pass 1's derived key %q must not already exist", rec.IdempotencyKey)
	}
	for _, rec := range first {
		_, err := l.Append(rec)
		require.NoError(t, err)
	}

	headAfterFirst, ok, err := st.Head()
	require.NoError(t, err)
	require.True(t, ok)
	rowsAfterFirst := rowCount(t, st)

	// Every claim pass 1 derived is now IN the ledger -- its key is stored.
	for _, rec := range first {
		_, present, err := st.ByIdemKey(rec.IdempotencyKey)
		require.NoError(t, err)
		assert.True(t, present, "claim %s's key must be stored after pass 1", rec.ID)
	}

	// PASS 2: fold the SAME store and ledger again. It must still DERIVE the
	// claims (a pass that produced nothing would make "appends nothing"
	// vacuous), and every derived key must already exist.
	second, err := rc.Reconcile(ctx, Window{})
	require.NoError(t, err)
	require.Len(t, second, len(first),
		"the second pass must DERIVE the same number of claims: it cannot pass by producing nothing")
	assert.ElementsMatch(t, claimIDs(first), claimIDs(second),
		"the second pass must re-derive the identical claim set over unchanged state")

	for _, rec := range second {
		_, present, err := st.ByIdemKey(rec.IdempotencyKey)
		require.NoError(t, err)
		require.True(t, present, "every key the second pass derived must already exist")
		id, err := l.Append(rec)
		require.NoError(t, err)
		assert.Equal(t, rec.ID, id,
			"ledger.Append must return the EXISTING record's id, not write a new one")
	}

	headAfterSecond, ok, err := st.Head()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, headAfterFirst.Seq, headAfterSecond.Seq, "the second pass must not advance the chain position")
	assert.Equal(t, headAfterFirst.Hash, headAfterSecond.Hash, "the second pass must not change the head hash")
	assert.Equal(t, rowsAfterFirst, rowCount(t, st), "the second pass must append zero rows")
}

// ---------------------------------------------------------------------------
// The fixpoint is reached in ONE pass, including when the first pass discovers a
// memory by CONTENT MATCH rather than by id.
// ---------------------------------------------------------------------------

// TestContentMatchSecondPassAppendsZeroRecords pins the headline idempotence
// property on the content-match path -- the one the id-match fixture does not
// exercise.
//
// Scenario: the add produced mem-produced, which is ABSENT from the complete
// listing, but a listed memory (mem-other) carries the add's submitted content,
// so the first pass infers kept_by_content_match for it. That inference's
// SUBJECT is mem-other, so a second pass folds it back in as a KNOWN memory --
// and because mem-other IS present in the listing, a producer that only emitted
// the Reconstructed claim on the first pass would append a fresh Observed
// stored_by_mem0 claim on the second. The fixpoint would then take TWO passes,
// breaking the documented promise that re-running against an unchanged store
// appends nothing.
//
// The fix makes the first pass ALSO emit the Observed stored_by_mem0 claim for
// the matched memory, since that memory is present in the very enumeration being
// read -- so the fixpoint is reached in one pass. This test asserts BOTH halves
// so it cannot pass by producing nothing: the second pass DERIVES a non-empty
// claim set identical to the first, and appending each second-pass claim returns
// the EXISTING record, leaving Seq, head hash and row count unchanged.
func TestContentMatchSecondPassAppendsZeroRecords(t *testing.T) {
	st, l := newIntegrityLedger(t)
	scope := record.Scope{UserID: "u1"}
	appendAll(t, l,
		addResolvedSeed(t, "add_resolved:stored_by_mem0:evt-1", "evt-1", "mem-produced", scope, fixedTime),
	)

	// mem-produced is absent from the listing; mem-other carries the same
	// content the add submitted. mem-produced's history is ADD-only, so the
	// removal stage claims nothing (absence alone is not a removal).
	client, _ := removedClient(t,
		removedPage(mem0.Memory{ID: "mem-other", Memory: "hello world", UserID: "u1"}),
		func(memoryID string) (mem0.HistoryResponse, int) {
			return mem0.HistoryResponse{{
				ID:        "hist-1",
				MemoryID:  memoryID,
				NewMemory: strPtr("hello world"),
				Event:     "ADD",
				CreatedAt: "2024-01-01T12:00:00Z",
			}}, http.StatusOK
		},
	)
	rc := New(l, client)
	ctx := context.Background()

	// PASS 1: derive and append everything.
	first, err := rc.Reconcile(ctx, Window{})
	require.NoError(t, err)
	require.NotEmpty(t, first, "the fixture must derive at least one claim, or this test is vacuous")

	// The fixture must actually exercise the content-match branch, or this test
	// proves nothing about the path it exists to pin.
	var sawContentMatch bool
	for _, rec := range first {
		if rec.ID == record.RecordID("memory_kept:kept_by_content_match:mem-other") {
			sawContentMatch = true
		}
	}
	require.True(t, sawContentMatch,
		"the fixture must derive the kept_by_content_match claim, or it does not exercise the content-match path")

	for _, rec := range first {
		_, err := l.Append(rec)
		require.NoError(t, err)
	}
	headAfterFirst, ok, err := st.Head()
	require.NoError(t, err)
	require.True(t, ok)
	rowsAfterFirst := rowCount(t, st)

	// PASS 2: fold the SAME store and ledger again. It must still DERIVE the
	// claims (a pass that produced nothing would make "appends nothing"
	// vacuous), and every derived key must already exist.
	second, err := rc.Reconcile(ctx, Window{})
	require.NoError(t, err)
	require.NotEmpty(t, second,
		"the second pass must derive a NON-EMPTY claim set: it cannot pass by producing nothing")
	require.Len(t, second, len(first),
		"the second pass must DERIVE the same number of claims as the first; the content-match pass must reach the fixpoint in ONE pass")
	assert.ElementsMatch(t, claimIDs(first), claimIDs(second),
		"the second pass must re-derive the identical claim set over unchanged state")

	for _, rec := range second {
		_, present, err := st.ByIdemKey(rec.IdempotencyKey)
		require.NoError(t, err)
		require.True(t, present, "every key the second pass derived must already exist")
		id, err := l.Append(rec)
		require.NoError(t, err)
		assert.Equal(t, rec.ID, id, "ledger.Append must return the EXISTING record's id, not write a new one")
	}

	headAfterSecond, ok, err := st.Head()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, headAfterFirst.Seq, headAfterSecond.Seq, "the second pass must not advance the chain position")
	assert.Equal(t, headAfterFirst.Hash, headAfterSecond.Hash, "the second pass must not change the head hash")
	assert.Equal(t, rowsAfterFirst, rowCount(t, st), "the second pass must append zero rows")
}

// ---------------------------------------------------------------------------
// A dry run and the real pass that follows it write exactly the same claims.
// ---------------------------------------------------------------------------

// TestDryRunThenRealPassIsStable pins that Reconcile is the dry run: it computes
// and returns the claims and writes NOTHING itself, so a dry run followed by a
// real pass that appends the reported claims writes exactly those claims and no
// others. This is the property the CLI's --dry-run relies on.
func TestDryRunThenRealPassIsStable(t *testing.T) {
	st, l := newIntegrityLedger(t)
	seedKnownMemoryAndCoveringSearch(t, l)
	client := enumerationClient(t, completePage(mem0.Memory{ID: "mem-1", Memory: "hello world", UserID: "u1"}))
	rc := New(l, client)

	rowsBefore := rowCount(t, st)

	// The dry run: Reconcile computes the claims and writes nothing.
	reported, err := rc.Reconcile(context.Background(), Window{})
	require.NoError(t, err)
	require.NotEmpty(t, reported, "the fixture must report at least one claim, or the comparison is vacuous")
	assert.Equal(t, rowsBefore, rowCount(t, st), "a dry run must write nothing")

	// The real pass: append exactly what the dry run reported.
	appended := make([]record.RecordID, 0, len(reported))
	for _, rec := range reported {
		id, err := l.Append(rec)
		require.NoError(t, err)
		appended = append(appended, id)
	}

	assert.Equal(t, rowsBefore+len(reported), rowCount(t, st),
		"the real pass must write exactly the reported claims, no more and no fewer")
	assert.ElementsMatch(t, claimIDs(reported), appended,
		"the ids the real pass wrote must be exactly the ids the dry run reported")
	for _, rec := range reported {
		got, err := st.GetRecord(rec.ID)
		require.NoError(t, err, "every reported claim must now be in the ledger")
		assert.Equal(t, rec.ID, got.ID)
	}
}

// ---------------------------------------------------------------------------
// An incomplete enumeration yields no absence claim and an error, never a
// partial conclusion.
// ---------------------------------------------------------------------------

// TestNoAbsenceClaimIsProducedFromAnIncompleteEnumeration is the integrative
// guarantee behind the enumeration's completeness check: when Mem0's get_all
// walk contradicts its own count, the pass must FAIL and derive NOTHING. The
// absence paths (memory_kept, removed_by_mem0) read presence only from a
// PROVEN-exhaustive listing, so a short read must never be mistaken for a
// smaller complete scope and become a fabricated, permanently signed absence.
func TestNoAbsenceClaimIsProducedFromAnIncompleteEnumeration(t *testing.T) {
	_, l := newIntegrityLedger(t)
	scope := record.Scope{UserID: "u1"}
	appendAll(t, l,
		addResolvedSeed(t, "add_resolved:stored_by_mem0:evt-1", "evt-1", "mem-1", scope, fixedTime),
	)

	// The first page reports count 2 but serves a single result and no next
	// page: the walk collects 1 of a reported 2, so GetAllComplete fails with
	// ErrEnumerationIncomplete.
	client := enumerationClient(t, func(int, *http.Request) enumPage {
		return enumPage{Count: 2, Results: []mem0.Memory{{ID: "mem-1", Memory: "hello world", UserID: "u1"}}}
	})
	rc := New(l, client)

	got, err := rc.Reconcile(context.Background(), Window{})
	require.Error(t, err, "an enumeration that disagrees with its own count must fail the pass loudly")
	assert.ErrorIs(t, err, mem0.ErrEnumerationIncomplete,
		"the incomplete-enumeration sentinel must survive wrapping")
	assert.Empty(t, got,
		"no absence (or any other) claim may be derived from an incomplete enumeration -- a partial read must never be a conclusion")
}

// ---------------------------------------------------------------------------
// Every Reconstructed claim names a registered rule, and every registered rule
// carries the current v1 version.
// ---------------------------------------------------------------------------

// TestEveryReconstructedClaimNamesARegistryRule walks every record a pass
// produces and asserts that each Reconstructed claim's rule and version exist in
// the registry and that its basis is non-empty. A Reconstructed claim rests on
// an inference, so the rule that made it is the reader's only account of WHY the
// record is true; an unregistered or mis-versioned name would make a signed,
// permanent record unauditable.
//
// It also folds in the Task 4 review finding: every rule the registry exposes
// must carry exactly the current v1 version constant. Nothing asserted that
// before, so a silent version drift in a permanent, SIGNED value would have left
// the whole suite green. The assertion references the registry's OWN constant,
// so the version has a single source of truth rather than a string repeated at
// the test.
func TestEveryReconstructedClaimNamesARegistryRule(t *testing.T) {
	_, l := newIntegrityLedger(t)
	scope := record.Scope{UserID: "u1"}

	// An unresolved add whose Mem0 event resolved SUCCEEDED with no results
	// yields a Reconstructed add_resolved (no_facts_extracted); a known memory
	// not returned by a later covering search yields a Reconstructed
	// memory_dropped (absent_from_search).
	appendAll(t, l,
		mustAddRequested(t, "r-add-nf", "evt-nf", fixedTime),
		addResolvedSeed(t, "add_resolved:stored_by_mem0:evt-1", "evt-1", "mem-1", scope, fixedTime),
		searchRecord(t, "search-1", scope,
			mem0.SearchPerformedPayload{Query: "q", TopK: 5, Count: 0}, fixedTime.Add(time.Minute)),
	)
	rc := New(l, integrityProvenanceClient(t))

	got, err := rc.Reconcile(context.Background(), Window{})
	require.NoError(t, err)

	var reconstructed []record.Record
	for _, rec := range got {
		if rec.Reason.Tier() == record.Reconstructed {
			reconstructed = append(reconstructed, rec)
		}
	}
	require.NotEmpty(t, reconstructed, "the fixture must produce at least one Reconstructed claim, or the walk is vacuous")

	rulesSeen := make(map[string]bool)
	for _, rec := range reconstructed {
		// The record package exposes no accessor for the reconstructed rule,
		// version or basis, so the evidence is asserted on its encoded bytes.
		encoded, err := rec.Reason.Encode()
		require.NoError(t, err)
		var wire struct {
			Reconstructed struct {
				Basis       []string `json:"basis"`
				Rule        string   `json:"rule"`
				RuleVersion string   `json:"rule_version"`
			} `json:"reconstructed"`
		}
		require.NoError(t, json.Unmarshal(encoded, &wire))

		rule, ok := LookupRule(wire.Reconstructed.Rule)
		require.True(t, ok,
			"claim %s names rule %q, which must be in the registry", rec.ID, wire.Reconstructed.Rule)
		assert.Equal(t, rule.Name, wire.Reconstructed.Rule,
			"the recorded rule must name the registry entry")
		assert.Equal(t, rule.Version, wire.Reconstructed.RuleVersion,
			"the recorded rule version must be the registry's version")
		assert.NotEmpty(t, wire.Reconstructed.Basis,
			"claim %s is an inference and must carry a non-empty basis", rec.ID)
		rulesSeen[rule.Name] = true
	}

	// The walk must actually exercise more than one rule -- otherwise it could
	// pass over a single rule and hide a drift in the others.
	assert.True(t, rulesSeen[RuleNoFactsExtracted],
		"the no_facts_extracted rule must be exercised by the walk")
	assert.True(t, rulesSeen[RuleAbsentFromSearch],
		"the absent_from_search rule must be exercised by the walk")

	// The folded-in finding: every registered rule's Version is exactly the
	// current v1 constant, referenced here rather than re-spelled.
	for _, rule := range Rules() {
		assert.Equal(t, ruleVersionV1, rule.Version,
			"rule %q must carry the current v1 version", rule.Name)
	}
}

// integrityProvenanceClient serves the three endpoints the provenance pass
// reads: a COMPLETE empty scope listing, the SUCCEEDED-but-empty event status
// that resolves the add, and an ADD-only history that does NOT corroborate a
// removal. It never touches the network.
func integrityProvenanceClient(t *testing.T) *mem0.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v3/memories/":
			// A complete, EMPTY listing: proven exhaustive and holding no
			// memory, so resolveKept claims nothing for mem-1 and
			// resolveRemoved must consult mem-1's history.
			writeRemovedJSON(w, enumPage{Count: 0, Results: nil})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/event/evt-nf/":
			writeRemovedJSON(w, mem0.EventStatusResponse{ID: "evt-nf", Status: "SUCCEEDED", Results: nil})
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
	return mem0.NewClient(srv.URL, "test-key", nil)
}

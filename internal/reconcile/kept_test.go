package reconcile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/mem0"
	"notary/internal/record"
)

// ---------------------------------------------------------------------------
// Fixtures: a paginated get_all server, exactly as Mem0 shapes it.
// ---------------------------------------------------------------------------

// enumPage is the get_all response body a test server serves. It mirrors
// mem0.GetAllResponse, repeated here so a test can control count, next and
// results independently of the client's own type.
type enumPage struct {
	Count   int           `json:"count"`
	Next    *string       `json:"next"`
	Results []mem0.Memory `json:"results"`
}

// enumerationClient starts an httptest server that answers every POST
// /v3/memories/ page from handler, keyed on the page query parameter. It never
// touches the network.
func enumerationClient(t *testing.T, handler func(page int, r *http.Request) enumPage) *mem0.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		body, err := json.Marshal(handler(page, r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return mem0.NewClient(srv.URL, "test-key", nil)
}

// completePage serves a single, self-consistent page: count == len(results) and
// next == null, so GetAllComplete certifies the listing exhaustive.
func completePage(mems ...mem0.Memory) func(int, *http.Request) enumPage {
	return func(int, *http.Request) enumPage {
		return enumPage{Count: len(mems), Results: mems}
	}
}

// errorEnumerationClient answers every get_all request with a non-2xx status, so
// a test can prove a Mem0 outage fails the pass rather than being mistaken for
// an absent memory.
func errorEnumerationClient(t *testing.T) *mem0.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"mem0 outage"}`))
	}))
	t.Cleanup(srv.Close)
	return mem0.NewClient(srv.URL, "test-key", nil)
}

// The fixture digests come from the SHARED scheme, record.ContentHash, which both
// the interceptor and this reconciler now call. They are deliberately NOT
// re-pinned as literals here: internal/record/content_test.go is the single place
// that pins the scheme's exact bytes, so one test covers both sides and a drift
// cannot leave this file green while the shared scheme has moved. (helloWorldHash
// is the digest of the text both the add and the listed memory carry;
// differentMemoryHash is an unrelated digest, used to prove the id match takes
// precedence over the content match.)
var (
	helloWorldHash      = record.ContentHash("hello world")
	differentMemoryHash = record.ContentHash("a different memory")
)

// firstKept calls resolveKept with a pre-built complete enumeration and requires
// it to yield exactly one record that ledger.Append would accept (a valid
// claim). The content-match path yields TWO records and is asserted directly in
// TestKeptReconstructedWhenOnlyContentHashMatches rather than through here.
func firstKept(t *testing.T, rc *Reconciler, scope record.Scope, enum mem0.CompleteEnumeration, known []knownMemory) record.Record {
	t.Helper()
	got, err := rc.resolveKept(scope, enum, known)
	require.NoError(t, err)
	require.Len(t, got, 1, "expected exactly one produced memory_kept record")
	require.NoError(t, got[0].Validate(), "the produced record must be a valid claim the caller can append")
	return got[0]
}

// keptScope is the single entity scope the fixtures live in.
var keptScope = record.Scope{UserID: "u1"}

// keptAddRecordID is the id of the add_resolved record that produced the
// fixture memory; it is the basis a reconstructed claim rests on.
const keptAddRecordID = record.RecordID("add_resolved:stored_by_mem0:evt-1")

// enumerateKeptScope builds the complete enumeration of keptScope through c --
// the only constructor of a CompleteEnumeration -- so a producer test never
// hand-writes one.
func enumerateKeptScope(t *testing.T, c *mem0.Client) mem0.CompleteEnumeration {
	t.Helper()
	enum, err := c.GetAllComplete(context.Background(), mem0.GetAllRequest{Filters: scopeFilters(keptScope)})
	require.NoError(t, err)
	require.True(t, enum.Valid())
	return enum
}

// ---------------------------------------------------------------------------
// Spec §5 rows 4-5: memory_kept from a complete enumeration of the scope.
// ---------------------------------------------------------------------------

// TestKeptObservedWhenMemoryIDMatches covers row 4: the add's produced memory id
// is present in the complete enumeration, so the presence is Observed and the
// reason kind is stored_by_mem0, exactly as the add_resolved row uses it.
func TestKeptObservedWhenMemoryIDMatches(t *testing.T) {
	listed := mem0.Memory{ID: "mem-1", Memory: "hello world", UserID: "u1"}
	known := knownMemory{
		MemoryID:    "mem-1",
		Scope:       keptScope,
		ContentHash: differentMemoryHash,
		Basis:       keptAddRecordID,
		At:          fixedTime,
	}
	client := enumerationClient(t, completePage(listed))
	rc := New(&fakeReader{}, client)
	enum := enumerateKeptScope(t, client)

	rec := firstKept(t, rc, keptScope, enum, []knownMemory{known})

	assert.Equal(t, record.EventMemoryKept, rec.Event)
	assert.Equal(t, record.ReasonStoredByMem0, rec.Reason.Kind())
	assert.Equal(t, record.Observed, rec.Reason.Tier(),
		"the id matched a listed memory: Mem0 reported that presence directly, so the claim is Observed")
	assert.Equal(t, keptScope, rec.Subject.Scope)
	assert.Equal(t, "mem-1", rec.Subject.MemoryID)
	assert.Equal(t, helloWorldHash, rec.Subject.ContentHash,
		"the subject content hash is the LISTED memory's digest, via the shared record.ContentHash scheme")
	assert.Equal(t, record.RecordID("memory_kept:stored_by_mem0:mem-1"), rec.ID,
		"the record id is qualified by the reason kind, as records.id is UNIQUE while duplicate suppression matches only the idempotency key")
	assert.Equal(t, fixedTime, rec.At, "At is the add event's time")
	assert.True(t, rec.RecordedAt.IsZero(), "RecordedAt is left zero for ledger.Append to stamp")

	// The Observed evidence is the listing entry Mem0 returned for the memory.
	ev, ok := rec.Reason.Observed()
	require.True(t, ok)
	var back mem0.Memory
	require.NoError(t, json.Unmarshal(ev.Payload(), &back))
	assert.Equal(t, listed.ID, back.ID)
	assert.Equal(t, listed.Memory, back.Memory)

	assert.NotEmpty(t, rec.IdempotencyKey)
	assert.Equal(t, rec.IdempotencyKey, firstKept(t, rc, keptScope, enum, []knownMemory{known}).IdempotencyKey,
		"a second identical call must re-derive the identical key so ledger.Append suppresses the duplicate")
}

// keptOfKind returns the single produced record of the given reason kind.
func keptOfKind(t *testing.T, recs []record.Record, kind record.ReasonKind) record.Record {
	t.Helper()
	var found []record.Record
	for _, r := range recs {
		if r.Reason.Kind() == kind {
			found = append(found, r)
		}
	}
	require.Len(t, found, 1, "expected exactly one produced record of kind %q", kind)
	return found[0]
}

// TestKeptReconstructedWhenOnlyContentHashMatches covers row 5: the produced id
// is absent from the complete enumeration, but a listed memory's content hash
// equals the add's submitted text, so the memory is kept under a different id.
// The claim is an inference and must say so -- and, because the matched memory
// is present in the very enumeration being read, the same call ALSO emits the
// Observed stored_by_mem0 claim for it, so the fixpoint is reached in one pass
// (see keptRecords).
func TestKeptReconstructedWhenOnlyContentHashMatches(t *testing.T) {
	listed := mem0.Memory{ID: "mem-other", Memory: "hello world", UserID: "u1"}
	known := knownMemory{
		MemoryID:    "mem-produced",
		Scope:       keptScope,
		ContentHash: helloWorldHash,
		Basis:       keptAddRecordID,
		At:          fixedTime,
	}
	client := enumerationClient(t, completePage(listed))
	rc := New(&fakeReader{}, client)
	enum := enumerateKeptScope(t, client)

	got, err := rc.resolveKept(keptScope, enum, []knownMemory{known})
	require.NoError(t, err)
	require.Len(t, got, 2,
		"a content match emits BOTH the Reconstructed inference and the Observed presence of the matched memory, so the fixpoint is reached in one pass")

	rec := keptOfKind(t, got, record.ReasonKeptByContentMatch)
	observed := keptOfKind(t, got, record.ReasonStoredByMem0)
	assert.Equal(t, record.EventMemoryKept, observed.Event)
	assert.Equal(t, record.Observed, observed.Reason.Tier(),
		"the matched memory is present in the enumeration being read, so its presence is an Observed fact")
	assert.Equal(t, "mem-other", observed.Subject.MemoryID)
	assert.Equal(t, record.RecordID("memory_kept:stored_by_mem0:mem-other"), observed.ID)

	assert.Equal(t, record.EventMemoryKept, rec.Event)
	assert.Equal(t, record.ReasonKeptByContentMatch, rec.Reason.Kind())
	assert.Equal(t, record.Reconstructed, rec.Reason.Tier(),
		"an inference from a content match is Reconstructed, not an observation")
	assert.Equal(t, record.RecordID("memory_kept:kept_by_content_match:mem-other"), rec.ID,
		"the record id is qualified by the reason kind")
	assert.Equal(t, "mem-other", rec.Subject.MemoryID,
		"the subject is the memory that actually exists in the listing, not the absent produced id")
	assert.Equal(t, helloWorldHash, rec.Subject.ContentHash)
	assert.Equal(t, fixedTime, rec.At, "At is the add event's time")
	assert.True(t, rec.RecordedAt.IsZero(), "RecordedAt is left zero for ledger.Append to stamp")

	// The record package exposes no accessor for basis/rule/version/confidence,
	// so the reconstructed evidence is asserted on its encoded bytes.
	encoded, err := rec.Reason.Encode()
	require.NoError(t, err)
	var wire struct {
		Reconstructed struct {
			Basis       []string `json:"basis"`
			Rule        string   `json:"rule"`
			RuleVersion string   `json:"rule_version"`
			Confidence  *float64 `json:"confidence"`
		} `json:"reconstructed"`
	}
	require.NoError(t, json.Unmarshal(encoded, &wire))

	rule, ok := LookupRule(RuleKeptByContentMatch)
	require.True(t, ok, "the rule must be in the registry")
	assert.Equal(t, rule.Name, wire.Reconstructed.Rule)
	assert.Equal(t, "1", wire.Reconstructed.RuleVersion)
	assert.Equal(t, rule.Version, wire.Reconstructed.RuleVersion)
	require.NotEmpty(t, wire.Reconstructed.Basis, "a reconstructed claim must carry a basis")
	assert.Equal(t, []string{string(keptAddRecordID)}, wire.Reconstructed.Basis,
		"the basis carries the add record's id")
	assert.Nil(t, wire.Reconstructed.Confidence,
		"confidence is left at ZERO, which the encoding omits rather than rendering as \"0% confident\"")

	assert.NotEmpty(t, rec.IdempotencyKey)
	again, err := rc.resolveKept(keptScope, enum, []knownMemory{known})
	require.NoError(t, err)
	assert.Equal(t, rec.IdempotencyKey, keptOfKind(t, again, record.ReasonKeptByContentMatch).IdempotencyKey,
		"a second identical call must re-derive the identical key")
}

// TestKeptWritesNothingWhenMemoryIsNeitherMatchedNorPresent pins the third row
// of the mapping: when neither the id nor a content hash matches, the producer
// must invent nothing.
func TestKeptWritesNothingWhenMemoryIsNeitherMatchedNorPresent(t *testing.T) {
	listed := mem0.Memory{ID: "mem-x", Memory: "a different memory", UserID: "u1"}
	known := knownMemory{
		MemoryID:    "mem-produced",
		Scope:       keptScope,
		ContentHash: helloWorldHash,
		Basis:       keptAddRecordID,
		At:          fixedTime,
	}
	client := enumerationClient(t, completePage(listed))
	rc := New(&fakeReader{}, client)
	enum := enumerateKeptScope(t, client)

	got, err := rc.resolveKept(keptScope, enum, []knownMemory{known})
	require.NoError(t, err, "a memory that is neither present nor content-matched is a clean non-event, not an error")
	assert.Empty(t, got, "nothing may be claimed when the memory is neither matched nor present")
}

// ---------------------------------------------------------------------------
// The shared enumeration step (Finding 2). resolveKept and resolveRemoved no
// longer fetch anything; enumerateScopes is now the ONE place a scope is walked
// per pass. The enumeration-failure coverage that used to live on resolveKept
// moves here with the code. enumerateScopes is exercised again end to end by
// TestNoAbsenceClaimIsProducedFromAnIncompleteEnumeration and
// TestReconcileRemovedFailsOnAnIncompleteEnumeration.
// ---------------------------------------------------------------------------

// scopesWorklist builds a worklist whose single scope is keptScope and whose
// known memories are known, so enumerateScopes will walk keptScope.
func scopesWorklist(known ...knownMemory) worklist {
	return worklist{scopes: []record.Scope{keptScope}, known: known}
}

// knownInKeptScope is a known memory in keptScope, so a scope becomes a subject
// for enumeration.
func knownInKeptScope() knownMemory {
	return knownMemory{
		MemoryID:    "mem-produced",
		Scope:       keptScope,
		ContentHash: helloWorldHash,
		Basis:       keptAddRecordID,
		At:          fixedTime,
	}
}

// TestEnumerateScopesOnlyClaimsFromACompleteEnumeration is the load-bearing test
// for requirement 1: a first page that omits a memory while COUNT reports it
// exists cannot certify the scope exhaustive, so the walk must ERROR and return
// NO enumeration. An incomplete read must never look like a (smaller) complete
// scope.
func TestEnumerateScopesOnlyClaimsFromACompleteEnumeration(t *testing.T) {
	// The first page reports count 1 (the memory exists) but serves no results
	// and no next page: the walk collects 0 of a reported 1.
	client := enumerationClient(t, func(int, *http.Request) enumPage {
		return enumPage{Count: 1, Results: nil}
	})
	rc := New(&fakeReader{}, client)

	enums, err := rc.enumerateScopes(context.Background(), scopesWorklist(knownInKeptScope()))

	require.Error(t, err, "an enumeration that disagrees with its own count must fail the pass loudly")
	assert.ErrorIs(t, err, mem0.ErrEnumerationIncomplete, "the incomplete-enumeration error must survive wrapping")
	assert.Empty(t, enums, "no enumeration may be returned from an incomplete walk")
}

// TestEnumerateScopesPropagatesEnumerationError pins that a Mem0 outage fails
// loudly and returns nothing, so it can never be mistaken for an absent memory.
func TestEnumerateScopesPropagatesEnumerationError(t *testing.T) {
	rc := New(&fakeReader{}, errorEnumerationClient(t))

	enums, err := rc.enumerateScopes(context.Background(), scopesWorklist(knownInKeptScope()))

	require.Error(t, err, "a Mem0 outage must fail loudly, never be misread as an absent memory")
	var httpErr *mem0.HTTPError
	require.ErrorAs(t, err, &httpErr, "the underlying Mem0 error must survive wrapping")
	assert.Equal(t, http.StatusInternalServerError, httpErr.StatusCode)
	assert.Empty(t, enums, "no enumeration may be returned on a Mem0 error")
}

// TestEnumerateScopesFailsLoudlyWithoutAClient mirrors resolveAdd: a nil Mem0
// client is a misconfigured reconciler, not a "nothing to claim" state.
func TestEnumerateScopesFailsLoudlyWithoutAClient(t *testing.T) {
	rc := New(&fakeReader{}, nil)

	enums, err := rc.enumerateScopes(context.Background(), scopesWorklist(knownInKeptScope()))

	require.Error(t, err, "a nil client is a misconfigured reconciler, not a clean pass")
	assert.ErrorIs(t, err, ErrNoMem0Client)
	assert.Empty(t, enums)
}

// TestEnumerateScopesSkipsScopeWithNoKnownMemory pins the early exit: with no
// known memory in the scope there is no subject to claim about, so the walk must
// not make a Mem0 call at all (proved here by a nil client, which would
// otherwise fail the pass loudly).
func TestEnumerateScopesSkipsScopeWithNoKnownMemory(t *testing.T) {
	other := knownMemory{MemoryID: "mem-other", Scope: record.Scope{UserID: "u2"}, ContentHash: testHash(0x33), Basis: "r-other"}
	rc := New(&fakeReader{}, nil)

	enums, err := rc.enumerateScopes(context.Background(), scopesWorklist(other))
	require.NoError(t, err, "a scope with no known memory has nothing to enumerate and must not touch Mem0")
	assert.Empty(t, enums)

	// resolveKept likewise writes nothing for a scope with no known memory,
	// without touching its (invalid) enumeration.
	got, err := rc.resolveKept(keptScope, mem0.CompleteEnumeration{}, []knownMemory{other})
	require.NoError(t, err, "a scope with no known memory has nothing to resolve")
	assert.Empty(t, got)
}

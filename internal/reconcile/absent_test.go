package reconcile

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/mem0"
	"notary/internal/record"
)

// ---------------------------------------------------------------------------
// Fixtures: a known memory and a search_performed record, built the way the
// interceptor writes them. The producer needs no Mem0 client at all -- every
// input it reads is already in the ledger -- so these tests never touch the
// network and never construct a client.
// ---------------------------------------------------------------------------

// absentScope is the single entity scope the absence fixtures live in.
var absentScope = record.Scope{UserID: "u1"}

// absentKnown is a memory the ledger establishes exists: an id, the scope it
// lives in, its content hash, and the establishing listing record's id (the
// basis an absence claim rests on).
func absentKnown(memoryID string, scope record.Scope) knownMemory {
	return knownMemory{
		MemoryID:    memoryID,
		Scope:       scope,
		ContentHash: record.ContentHash("hello world"),
		Basis:       record.RecordID("memory_kept:stored_by_mem0:" + memoryID),
		At:          fixedTime,
	}
}

// searchRecord builds a search_performed record whose Observed evidence is the
// SearchPerformedPayload carrying the query parameters, exactly as the
// interceptor writes it. TopK and Count are the two fields the saturation
// predicate reads.
func searchRecord(t *testing.T, id string, scope record.Scope, p mem0.SearchPerformedPayload, at time.Time) record.Record {
	t.Helper()
	return record.Record{
		ID:         record.RecordID(id),
		At:         at,
		RecordedAt: at,
		Event:      record.EventSearchPerformed,
		Reason:     observedReason(t, record.ReasonSearchPerformed, p),
		Subject:    record.Subject{Scope: scope, ContentHash: record.ContentHash(p.Query)},
	}
}

// surfacedRecord builds the memory_surfaced record the interceptor writes for
// the rank-th result of the search with correlation id searchID. Its id is
// "<searchID>#<rank>" and the returned memory's id rides on Subject.MemoryID --
// the exact linkage resolveAbsent reads to tell that a search returned a
// memory.
func surfacedRecord(t *testing.T, searchID string, rank int, memoryID string, scope record.Scope, at time.Time) record.Record {
	t.Helper()
	return record.Record{
		ID:     record.RecordID(fmt.Sprintf("%s#%d", searchID, rank)),
		At:     at,
		Event:  record.EventMemorySurfaced,
		Reason: observedReason(t, record.ReasonReturnedBySearch, mem0.MemorySurfacedPayload{Score: 0.9, Rank: rank}),
		Subject: record.Subject{
			MemoryID:    memoryID,
			Scope:       scope,
			ContentHash: record.ContentHash("surfaced " + memoryID),
		},
	}
}

// ---------------------------------------------------------------------------
// Spec §5 row 6: memory_dropped + absent_from_search only from a SATURATED
// search (Count < TopK).
// ---------------------------------------------------------------------------

// TestAbsentClaimedWhenResultsAreFewerThanTopK is the positive case: a search
// returned 2 of a top_k of 5, so the store had nothing further above the
// threshold to give, and a memory known to exist in that scope that the search
// did not return is a real, explainable drop.
func TestAbsentClaimedWhenResultsAreFewerThanTopK(t *testing.T) {
	known := absentKnown("mem-1", absentScope)
	at := fixedTime.Add(time.Minute)
	search := searchRecord(t, "search-1", absentScope, mem0.SearchPerformedPayload{
		Query: "q", TopK: 5, Count: 2,
	}, at)
	// The search returned two OTHER memories; mem-1 was not among them. The
	// non-return check is per memory, so a DIFFERENT memory surfacing must not
	// suppress mem-1's claim.
	surfaced := []record.Record{
		surfacedRecord(t, "search-1", 1, "mem-other", absentScope, at),
		surfacedRecord(t, "search-1", 2, "mem-other-2", absentScope, at),
	}
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveAbsent([]knownMemory{known}, []record.Record{search}, surfaced)
	require.NoError(t, err)
	require.Len(t, got, 1, "a known memory absent from a saturated search warrants exactly one memory_dropped claim")

	rec := got[0]
	require.NoError(t, rec.Validate(), "the produced record must be a valid claim the caller can append")
	assert.Equal(t, record.EventMemoryDropped, rec.Event)
	assert.Equal(t, record.ReasonAbsentFromSearch, rec.Reason.Kind())
	assert.Equal(t, record.Reconstructed, rec.Reason.Tier(),
		"the absence is an inference from the search's saturation, not an observation")
	assert.Equal(t, "mem-1", rec.Subject.MemoryID)
	assert.Equal(t, absentScope, rec.Subject.Scope)
	assert.Equal(t, record.RecordID("memory_dropped:absent_from_search:mem-1"), rec.ID,
		"the record id is qualified by the reason kind, as records.id is UNIQUE while duplicate suppression matches only the idempotency key")
	assert.Equal(t, fixedTime.Add(time.Minute), rec.At,
		"At is the COVERING SEARCH's time: the event being described is the search that failed to return the memory")
	assert.True(t, rec.RecordedAt.IsZero(), "RecordedAt is left zero for ledger.Append to stamp")
}

// TestAbsentClaimedWhenResultsAreEmptyAndTopKPositive pins the empty-result
// edge: 0 < 5 is a covering search (the store had nothing above the threshold
// at all), so absence is still warranted.
func TestAbsentClaimedWhenResultsAreEmptyAndTopKPositive(t *testing.T) {
	known := absentKnown("mem-1", absentScope)
	search := searchRecord(t, "search-1", absentScope, mem0.SearchPerformedPayload{
		Query: "q", TopK: 5, Count: 0,
	}, fixedTime.Add(time.Minute))
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveAbsent([]knownMemory{known}, []record.Record{search}, nil)
	require.NoError(t, err)
	require.Len(t, got, 1, "a search that returned nothing of a positive top_k is saturated and covers the memory")
}

// TestAbsentNotClaimedWhenResultsFillTopK is the load-bearing negative: when
// len(results) == top_k the result window was TRUNCATED by top_k, so absence
// proves nothing at all and no claim may be written. An implementation that
// wrote <= would fire here.
func TestAbsentNotClaimedWhenResultsFillTopK(t *testing.T) {
	known := absentKnown("mem-1", absentScope)
	search := searchRecord(t, "search-1", absentScope, mem0.SearchPerformedPayload{
		Query: "q", TopK: 5, Count: 5,
	}, fixedTime.Add(time.Minute))
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveAbsent([]knownMemory{known}, []record.Record{search}, nil)
	require.NoError(t, err)
	assert.Empty(t, got, "len(results) == top_k means the window was truncated by top_k, so absence proves nothing")
}

// TestAbsentNotClaimedWhenCoveringSearchReturnedTheMemory is the load-bearing
// positive-fact check on the RULE itself: when a saturated same-scope search
// actually RETURNED the memory, the memory did not drop under any reading of
// the claim (Count < top_k means the store returned everything above the
// threshold, so a returned memory was above the threshold). No absence claim
// may be written. A producer that checked only scope, time and saturation would
// fabricate an absence here -- the ordinary list-then-search workflow.
func TestAbsentNotClaimedWhenCoveringSearchReturnedTheMemory(t *testing.T) {
	known := absentKnown("mem-1", absentScope)
	at := fixedTime.Add(time.Minute)
	// The search is saturated (Count 2 < TopK 5) and one of its two results IS
	// mem-1, tied to it by the memory_surfaced record "search-1#1".
	search := searchRecord(t, "search-1", absentScope, mem0.SearchPerformedPayload{
		Query: "q", TopK: 5, Count: 2,
	}, at)
	surfaced := []record.Record{
		surfacedRecord(t, "search-1", 1, "mem-1", absentScope, at),
		surfacedRecord(t, "search-1", 2, "mem-2", absentScope, at),
	}
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveAbsent([]knownMemory{known}, []record.Record{search}, surfaced)
	require.NoError(t, err)
	assert.Empty(t, got,
		"the covering search RETURNED mem-1, so it did not drop: no absence claim may be written")
}

// TestAbsentNotClaimedWhenSurfacedEvidenceIsMissing is the load-bearing check
// for the residual false-claim class: a search reports Count > 0 results, but NO
// memory_surfaced records are present. The interceptor's surfaced writes are
// BEST-EFFORT by design (writeSurfaced returns early on any failure, which is
// exactly why the gap log exists), so a missing surfaced record is
// indistinguishable from a search that returned nothing. Absence must NOT be
// inferred from that missing evidence: the claim is withheld.
func TestAbsentNotClaimedWhenSurfacedEvidenceIsMissing(t *testing.T) {
	known := absentKnown("mem-1", absentScope)
	search := searchRecord(t, "search-1", absentScope, mem0.SearchPerformedPayload{
		Query: "q", TopK: 5, Count: 2,
	}, fixedTime.Add(time.Minute))
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveAbsent([]knownMemory{known}, []record.Record{search}, nil)
	require.NoError(t, err)
	assert.Empty(t, got,
		"Count 2 with no surfaced records does not account for the result count: non-return is unproven, so no claim")
}

// TestAbsentNotClaimedWhenSurfacedEvidenceIsShort: surfaced records exist but
// account for FEWER results than Count. The ledger is inconsistent for this
// search, so non-return is unproven and the claim is withheld -- absent
// evidence must not be read as "returned nothing".
func TestAbsentNotClaimedWhenSurfacedEvidenceIsShort(t *testing.T) {
	known := absentKnown("mem-1", absentScope)
	at := fixedTime.Add(time.Minute)
	search := searchRecord(t, "search-1", absentScope, mem0.SearchPerformedPayload{
		Query: "q", TopK: 5, Count: 3,
	}, at)
	// Only two of the three reported results have a surfaced record, and
	// neither is mem-1.
	surfaced := []record.Record{
		surfacedRecord(t, "search-1", 1, "mem-other", absentScope, at),
		surfacedRecord(t, "search-1", 2, "mem-other-2", absentScope, at),
	}
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveAbsent([]knownMemory{known}, []record.Record{search}, surfaced)
	require.NoError(t, err)
	assert.Empty(t, got,
		"2 surfaced records for Count 3 do not account for the result count: non-return is unproven, so no claim")
}

// TestAbsentNotClaimedWhenSurfacedEvidenceIsSurplus: surfaced records account
// for MORE results than Count. That is equally inconsistent, so the claim is
// withheld rather than fabricated from unmatched evidence.
func TestAbsentNotClaimedWhenSurfacedEvidenceIsSurplus(t *testing.T) {
	known := absentKnown("mem-1", absentScope)
	at := fixedTime.Add(time.Minute)
	search := searchRecord(t, "search-1", absentScope, mem0.SearchPerformedPayload{
		Query: "q", TopK: 5, Count: 1,
	}, at)
	surfaced := []record.Record{
		surfacedRecord(t, "search-1", 1, "mem-other", absentScope, at),
		surfacedRecord(t, "search-1", 2, "mem-other-2", absentScope, at),
	}
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveAbsent([]knownMemory{known}, []record.Record{search}, surfaced)
	require.NoError(t, err)
	assert.Empty(t, got,
		"2 surfaced records for Count 1 do not account for the result count: non-return is unproven, so no claim")
}

// TestAbsentNotClaimedWhenTopKIsZero pins the zero-top_k edge. top_k == 0 is
// treated as "not a covering search", NOT as "everything is absent": a caller
// who omitted top_k asked for no particular window, so a search cannot be read
// as evidence about any memory.
func TestAbsentNotClaimedWhenTopKIsZero(t *testing.T) {
	known := absentKnown("mem-1", absentScope)
	search := searchRecord(t, "search-1", absentScope, mem0.SearchPerformedPayload{
		Query: "q", TopK: 0, Count: 0,
	}, fixedTime.Add(time.Minute))
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveAbsent([]knownMemory{known}, []record.Record{search}, nil)
	require.NoError(t, err)
	assert.Empty(t, got, "a zero top_k is not a covering search and must not make every known memory look absent")
}

// TestAbsentNotClaimedForAMemoryThatWasNeverKnown is the eligibility rule: only
// a memory the ledger ESTABLISHED exists (a listing) may be claimed absent. A
// memory that merely appeared once in a search's results -- a memory_surfaced
// record, which does NOT establish existence -- must never produce an absence
// claim. This exercises the fold end to end: buildWorklist must withhold the
// ghost memory from `known`, and the producer must then claim nothing.
func TestAbsentNotClaimedForAMemoryThatWasNeverKnown(t *testing.T) {
	at := fixedTime.Add(time.Minute)
	// A memory that only ever surfaced in a search: it carries a concrete id,
	// but appearing once is not proof Notary knows it exists.
	surfaced := record.Record{
		ID:     "search-1#1",
		At:     at,
		Event:  record.EventMemorySurfaced,
		Reason: observedReason(t, record.ReasonReturnedBySearch, mem0.MemorySurfacedPayload{Score: 0.9, Rank: 1}),
		Subject: record.Subject{
			MemoryID:    "mem-ghost",
			Scope:       absentScope,
			ContentHash: record.ContentHash("ghost"),
		},
	}
	// ...and a saturated search in the same scope, which would cover the ghost
	// memory if it were a known one.
	search := searchRecord(t, "search-1", absentScope, mem0.SearchPerformedPayload{
		Query: "q", TopK: 5, Count: 1,
	}, at)

	wl, err := buildWorklist([]record.Record{surfaced, search})
	require.NoError(t, err)
	require.Empty(t, wl.known, "a memory that only ever surfaced in a search is not established by a listing")
	require.Len(t, wl.surfaced, 1, "the fold must carry the memory_surfaced records through to the producer")
	require.Len(t, wl.searches, 1)

	rc := New(&fakeReader{}, nil)
	got, err := rc.resolveAbsent(wl.known, wl.searches, wl.surfaced)
	require.NoError(t, err)
	assert.Empty(t, got, "a memory never established by a listing must never produce an absence claim")
}

// TestAbsentRequiresALaterSearchInTheSameScope pins the candidate rule: a
// search in a different scope, or one that predates the memory becoming known,
// is not a coverage candidate for that memory.
func TestAbsentRequiresALaterSearchInTheSameScope(t *testing.T) {
	known := absentKnown("mem-1", absentScope)

	otherScope := searchRecord(t, "search-other", record.Scope{UserID: "u2"}, mem0.SearchPerformedPayload{
		Query: "q", TopK: 5, Count: 1,
	}, fixedTime.Add(time.Minute))
	earlier := searchRecord(t, "search-earlier", absentScope, mem0.SearchPerformedPayload{
		Query: "q", TopK: 5, Count: 1,
	}, fixedTime.Add(-time.Minute))
	// Give each search surfaced evidence that accounts for its Count, so the ONLY
	// reason no claim is written is the scope/time filter under test -- not the
	// accounting requirement.
	surfaced := []record.Record{
		surfacedRecord(t, "search-other", 1, "mem-x", record.Scope{UserID: "u2"}, otherScope.At),
		surfacedRecord(t, "search-earlier", 1, "mem-y", absentScope, earlier.At),
	}

	rc := New(&fakeReader{}, nil)
	got, err := rc.resolveAbsent([]knownMemory{known}, []record.Record{otherScope, earlier}, surfaced)
	require.NoError(t, err)
	assert.Empty(t, got,
		"a search in another scope, or one that predates the memory, is not a coverage candidate")
}

// TestAbsentClaimRecordsRuleAndParameters pins the full shape of the claim: the
// event, tier, rule name and version, a basis naming BOTH the prior listing
// record and the covering search record, a confidence left at ZERO, and an
// idempotency key that is stable across two identical calls.
func TestAbsentClaimRecordsRuleAndParameters(t *testing.T) {
	known := absentKnown("mem-1", absentScope)
	at := fixedTime.Add(time.Minute)
	search := searchRecord(t, "search-1", absentScope, mem0.SearchPerformedPayload{
		Query: "q", TopK: 5, Count: 2,
	}, at)
	// The surfaced evidence accounts for the two reported results (neither is
	// mem-1), so the claim below rests on the rule, not on a missing record.
	surfaced := []record.Record{
		surfacedRecord(t, "search-1", 1, "mem-other", absentScope, at),
		surfacedRecord(t, "search-1", 2, "mem-other-2", absentScope, at),
	}
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveAbsent([]knownMemory{known}, []record.Record{search}, surfaced)
	require.NoError(t, err)
	require.Len(t, got, 1)
	rec := got[0]

	assert.Equal(t, record.EventMemoryDropped, rec.Event)
	assert.Equal(t, record.ReasonAbsentFromSearch, rec.Reason.Kind())
	assert.Equal(t, record.Reconstructed, rec.Reason.Tier())

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

	rule, ok := LookupRule(RuleAbsentFromSearch)
	require.True(t, ok, "the rule must be in the registry")
	assert.Equal(t, rule.Name, wire.Reconstructed.Rule)
	assert.Equal(t, "1", wire.Reconstructed.RuleVersion)
	assert.Equal(t, rule.Version, wire.Reconstructed.RuleVersion)
	require.NotEmpty(t, wire.Reconstructed.Basis, "a reconstructed claim must carry a basis")
	assert.Equal(t, []string{string(known.Basis), "search-1"}, wire.Reconstructed.Basis,
		"the basis names BOTH the prior listing record and the covering search record")
	assert.Nil(t, wire.Reconstructed.Confidence,
		"confidence is left at ZERO, which the encoding omits rather than rendering as \"0% confident\"")

	require.NotEmpty(t, rec.IdempotencyKey)
	again, err := rc.resolveAbsent([]knownMemory{known}, []record.Record{search}, surfaced)
	require.NoError(t, err)
	require.Len(t, again, 1)
	assert.Equal(t, rec.IdempotencyKey, again[0].IdempotencyKey,
		"a second identical call must re-derive the identical key so ledger.Append suppresses the duplicate")
}

// TestAbsentTwoCoveringSearchesYieldOneClaim states the deliberate, surprising
// consequence of keying a claim per MEMORY and per RULE: several later covering
// searches that each omit the same memory produce ONE absent_from_search claim,
// whose basis and At name the FIRST such covering search. The searches
// themselves remain recorded as search_performed, so a reader can still see
// they happened; only the derived claim is single.
func TestAbsentTwoCoveringSearchesYieldOneClaim(t *testing.T) {
	known := absentKnown("mem-1", absentScope)
	first := searchRecord(t, "search-first", absentScope, mem0.SearchPerformedPayload{
		Query: "q1", TopK: 5, Count: 1,
	}, fixedTime.Add(time.Minute))
	second := searchRecord(t, "search-second", absentScope, mem0.SearchPerformedPayload{
		Query: "q2", TopK: 5, Count: 3,
	}, fixedTime.Add(2*time.Minute))
	// Each search's surfaced evidence accounts for its own Count (1 and 3), so
	// both are genuine covering searches for mem-1, which neither returned.
	surfaced := []record.Record{
		surfacedRecord(t, "search-first", 1, "mem-f1", absentScope, first.At),
		surfacedRecord(t, "search-second", 1, "mem-s1", absentScope, second.At),
		surfacedRecord(t, "search-second", 2, "mem-s2", absentScope, second.At),
		surfacedRecord(t, "search-second", 3, "mem-s3", absentScope, second.At),
	}
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveAbsent([]knownMemory{known}, []record.Record{first, second}, surfaced)
	require.NoError(t, err)
	require.Len(t, got, 1, "the claim is keyed per memory, so two covering searches yield ONE claim")
	assert.Equal(t, record.RecordID("memory_dropped:absent_from_search:mem-1"), got[0].ID)
	assert.Equal(t, fixedTime.Add(time.Minute), got[0].At,
		"At is the FIRST covering search's time")

	encoded, err := got[0].Reason.Encode()
	require.NoError(t, err)
	var wire struct {
		Reconstructed struct {
			Basis []string `json:"basis"`
		} `json:"reconstructed"`
	}
	require.NoError(t, json.Unmarshal(encoded, &wire))
	assert.Equal(t, []string{string(known.Basis), "search-first"}, wire.Reconstructed.Basis,
		"the basis names the FIRST covering search")

	// The key must not depend on WHICH covering search was picked: the search
	// record id is deliberately absent from the claim identity.
	single, err := rc.resolveAbsent([]knownMemory{known}, []record.Record{first}, surfaced)
	require.NoError(t, err)
	require.Len(t, single, 1)
	assert.Equal(t, single[0].IdempotencyKey, got[0].IdempotencyKey,
		"the key is a function of the memory and the rule only, not of the particular covering search")
}

// TestAbsentSameMemoryInTwoScopesYieldsOneClaim pins the documented, deliberate
// consequence of a scope-less record id and idempotency key: the same Mem0
// memory id known in TWO scopes, each with a covering search, yields ONE
// memory_dropped claim and the second scope is silently dropped. The claim
// identity is per memory and per rule (spec §3 decision 4) and does NOT include
// the scope, so absentID ("memory_dropped:absent_from_search:<id>") and
// DeriveClaimIdemKey agree on one claim. This is accepted for v1 -- the memory
// is one object with one ledger identity -- and the test makes it read as a
// choice rather than an accident.
func TestAbsentSameMemoryInTwoScopesYieldsOneClaim(t *testing.T) {
	otherScope := record.Scope{UserID: "u2"}
	known := []knownMemory{
		absentKnown("mem-1", absentScope),
		absentKnown("mem-1", otherScope),
	}
	first := searchRecord(t, "search-u1", absentScope, mem0.SearchPerformedPayload{
		Query: "q1", TopK: 5, Count: 1,
	}, fixedTime.Add(time.Minute))
	second := searchRecord(t, "search-u2", otherScope, mem0.SearchPerformedPayload{
		Query: "q2", TopK: 5, Count: 1,
	}, fixedTime.Add(time.Minute))
	surfaced := []record.Record{
		surfacedRecord(t, "search-u1", 1, "mem-a", absentScope, first.At),
		surfacedRecord(t, "search-u2", 1, "mem-b", otherScope, second.At),
	}
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveAbsent(known, []record.Record{first, second}, surfaced)
	require.NoError(t, err)
	require.Len(t, got, 1,
		"the same memory id in two scopes derives one id and one key, so only one claim is emitted")
	assert.Equal(t, record.RecordID("memory_dropped:absent_from_search:mem-1"), got[0].ID)
	assert.Equal(t, absentScope, got[0].Subject.Scope,
		"the first scope's claim wins; the second is suppressed as a duplicate")
}

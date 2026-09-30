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

// The fixture digests come from the SHARED scheme, mem0.ContentHash, which both
// the interceptor and this reconciler now call. They are deliberately NOT
// re-pinned as literals here: internal/mem0/content_test.go is the single place
// that pins the scheme's exact bytes, so one test covers both sides and a drift
// cannot leave this file green while the shared scheme has moved. (helloWorldHash
// is the digest of the text both the add and the listed memory carry;
// differentMemoryHash is an unrelated digest, used to prove the id match takes
// precedence over the content match.)
var (
	helloWorldHash      = mem0.ContentHash("hello world")
	differentMemoryHash = mem0.ContentHash("a different memory")
)

// firstKept calls resolveKept and requires it to yield exactly one record that
// ledger.Append would accept (a valid claim).
func firstKept(t *testing.T, rc *Reconciler, scope record.Scope, known []knownMemory) record.Record {
	t.Helper()
	got, err := rc.resolveKept(context.Background(), scope, known)
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

	rec := firstKept(t, rc, keptScope, []knownMemory{known})

	assert.Equal(t, record.EventMemoryKept, rec.Event)
	assert.Equal(t, record.ReasonStoredByMem0, rec.Reason.Kind())
	assert.Equal(t, record.Observed, rec.Reason.Tier(),
		"the id matched a listed memory: Mem0 reported that presence directly, so the claim is Observed")
	assert.Equal(t, keptScope, rec.Subject.Scope)
	assert.Equal(t, "mem-1", rec.Subject.MemoryID)
	assert.Equal(t, helloWorldHash, rec.Subject.ContentHash,
		"the subject content hash is the LISTED memory's digest, via the shared mem0.ContentHash scheme")
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
	assert.Equal(t, rec.IdempotencyKey, firstKept(t, rc, keptScope, []knownMemory{known}).IdempotencyKey,
		"a second identical call must re-derive the identical key so ledger.Append suppresses the duplicate")
}

// TestKeptReconstructedWhenOnlyContentHashMatches covers row 5: the produced id
// is absent from the complete enumeration, but a listed memory's content hash
// equals the add's submitted text, so the memory is kept under a different id.
// The claim is an inference and must say so.
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

	rec := firstKept(t, rc, keptScope, []knownMemory{known})

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
	assert.Equal(t, rec.IdempotencyKey, firstKept(t, rc, keptScope, []knownMemory{known}).IdempotencyKey,
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

	got, err := rc.resolveKept(context.Background(), keptScope, []knownMemory{known})
	require.NoError(t, err, "a memory that is neither present nor content-matched is a clean non-event, not an error")
	assert.Empty(t, got, "nothing may be claimed when the memory is neither matched nor present")
}

// TestKeptOnlyClaimsFromACompleteEnumeration is the load-bearing test for
// requirement 1: a first page that omits the memory while COUNT reports it
// exists cannot certify the scope exhaustive, so the pass must ERROR and write
// NO memory_kept claim at all. An incomplete read must never look like a
// (smaller) complete scope.
func TestKeptOnlyClaimsFromACompleteEnumeration(t *testing.T) {
	known := knownMemory{
		MemoryID:    "mem-produced",
		Scope:       keptScope,
		ContentHash: helloWorldHash,
		Basis:       keptAddRecordID,
		At:          fixedTime,
	}
	// The first page reports count 1 (the memory exists) but serves no results
	// and no next page: the walk collects 0 of a reported 1.
	client := enumerationClient(t, func(int, *http.Request) enumPage {
		return enumPage{Count: 1, Results: nil}
	})
	rc := New(&fakeReader{}, client)

	got, err := rc.resolveKept(context.Background(), keptScope, []knownMemory{known})

	require.Error(t, err, "an enumeration that disagrees with its own count must fail the pass loudly")
	assert.ErrorIs(t, err, mem0.ErrEnumerationIncomplete, "the incomplete-enumeration error must survive wrapping")
	assert.Empty(t, got, "no memory_kept claim may be derived from an incomplete enumeration")
}

// TestKeptPropagatesEnumerationError pins that a Mem0 outage fails loudly and
// emits nothing, so it can never be mistaken for a memory that is absent.
func TestKeptPropagatesEnumerationError(t *testing.T) {
	known := knownMemory{
		MemoryID:    "mem-produced",
		Scope:       keptScope,
		ContentHash: helloWorldHash,
		Basis:       keptAddRecordID,
		At:          fixedTime,
	}
	rc := New(&fakeReader{}, errorEnumerationClient(t))

	got, err := rc.resolveKept(context.Background(), keptScope, []knownMemory{known})

	require.Error(t, err, "a Mem0 outage must fail loudly, never be misread as an absent memory")
	var httpErr *mem0.HTTPError
	require.ErrorAs(t, err, &httpErr, "the underlying Mem0 error must survive wrapping")
	assert.Equal(t, http.StatusInternalServerError, httpErr.StatusCode)
	assert.Empty(t, got, "no record may be emitted on a Mem0 error")
}

// TestKeptFailsLoudlyWithoutAClient mirrors resolveAdd: a nil Mem0 client is a
// misconfigured reconciler, not a "nothing to claim" state.
func TestKeptFailsLoudlyWithoutAClient(t *testing.T) {
	known := knownMemory{
		MemoryID:    "mem-produced",
		Scope:       keptScope,
		ContentHash: helloWorldHash,
		Basis:       keptAddRecordID,
		At:          fixedTime,
	}
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveKept(context.Background(), keptScope, []knownMemory{known})

	require.Error(t, err, "a nil client is a misconfigured reconciler, not a clean pass")
	assert.ErrorIs(t, err, ErrNoMem0Client)
	assert.Empty(t, got)
}

// TestKeptDoesNotEnumerateWhenNoMemoryIsKnown pins the early exit: with no known
// memory in the scope there is no subject to claim about, so the producer must
// not make a Mem0 call at all (proved here by a nil client, which would
// otherwise fail the pass loudly).
func TestKeptDoesNotEnumerateWhenNoMemoryIsKnown(t *testing.T) {
	other := knownMemory{MemoryID: "mem-other", Scope: record.Scope{UserID: "u2"}, ContentHash: testHash(0x33), Basis: "r-other"}
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveKept(context.Background(), keptScope, []knownMemory{other})
	require.NoError(t, err, "a scope with no known memory has nothing to resolve and must not touch Mem0")
	assert.Empty(t, got)
}

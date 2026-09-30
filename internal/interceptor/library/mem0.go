// Package library is the concrete interceptor that sits between a caller's
// Mem0 operation and Notary's audit ledger.
//
// It performs one Mem0 call, and -- when that call succeeds -- writes the
// Observed evidence it produced (an add acknowledgement, a search performed, or
// one memory surfaced) through interceptor.AuditWriter. The write path is
// strictly fail-open: the Mem0 result is returned to the caller with a nil
// error even when the record cannot be written, so an audit problem can never
// become a caller's problem. A failure of the Mem0 call itself is a real error
// and is returned unchanged, writing no record.
package library

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"notary/internal/interceptor"
	"notary/internal/mem0"
	"notary/internal/record"
)

// ErrMissingCorrelationID reports that Add or Search was called with an empty
// correlation ID. Notary never invents a correlation key -- least of all from a
// clock -- so an empty one is rejected before any Mem0 call is made. Callers
// with no natural identifier should derive one with DeriveCorrelationID.
var ErrMissingCorrelationID = errors.New("library: missing correlation ID")

// correlationDomain domain-separates DeriveCorrelationID's digest from every
// other use of SHA-256, so its output can never be confused with a bare hash of
// the same concatenated fields.
const correlationDomain = "notary/corr/v1"

// defaultMessageRole is the role Notary stamps on each message string it sends
// to Mem0's Add endpoint. Add's signature carries only content strings, so all
// of them are sent as user turns; a caller needing assistant or system turns
// must call mem0.Client directly.
const defaultMessageRole = "user"

// compile-time proof that *Mem0Interceptor satisfies interceptor.Interceptor.
var _ interceptor.Interceptor = (*Mem0Interceptor)(nil)

// Mem0Interceptor performs Mem0 operations and writes the Observed evidence for
// them to the audit ledger through a borrowed AuditWriter. It holds no state
// that changes between calls, so it is safe for concurrent use as long as its
// collaborators are.
type Mem0Interceptor struct {
	// mc is the Mem0 client. A nil client is reported as an error by Add and
	// Search rather than dereferenced, so a misconfiguration cannot panic.
	mc *mem0.Client
	// aw is the fail-open-loud write path. It is borrowed: Close closes it (the
	// AuditWriter owns the gap log) but never the ledger.
	aw *interceptor.AuditWriter
	// scope is the caller context stamped on every record this interceptor
	// writes.
	scope record.Scope
	// now supplies the clock used to stamp a record's At and RecordedAt. It is
	// never time.Now in the write path unless the caller supplied nil.
	now func() time.Time
}

// New returns a Mem0Interceptor that calls mc, writes through aw, stamps its
// records with scope, and reads its clock from now.
//
// A nil now falls back to time.Now, mirroring ledger.New: an unset clock must
// not panic, and it must not leave a record stamped with the zero time (which
// would make it invisible to a time-window read). A nil aw is tolerated -- the
// record is then simply not written, since there is no ledger to write it to.
//
// aw is borrowed. Close closes it, because the AuditWriter owns the gap log's
// lifetime; the ledger's lifetime belongs to the caller that built the
// AuditWriter.
func New(mc *mem0.Client, aw *interceptor.AuditWriter, scope record.Scope, now func() time.Time) *Mem0Interceptor {
	if now == nil {
		now = time.Now
	}
	return &Mem0Interceptor{mc: mc, aw: aw, scope: scope, now: now}
}

// FailMode reports the mode this interceptor runs under: always FailOpenLoud.
// The only mode v1 implements is fail-open-loud, so the audit failure is
// reported loudly (the AuditWriter's channels) and durably (the gap log) while
// the Mem0 result still reaches the caller.
func (m *Mem0Interceptor) FailMode() interceptor.FailMode { return interceptor.FailOpenLoud }

// Close closes the borrowed audit writer and returns its error. It is
// nil-safe: an interceptor with no audit writer closes cleanly, and calling
// Close more than once is safe. The ledger is never closed here -- the
// AuditWriter borrows it, so its lifetime belongs to the caller.
func (m *Mem0Interceptor) Close() error {
	if m == nil || m.aw == nil {
		return nil
	}
	return m.aw.Close()
}

// Add submits messages to Mem0's add endpoint and writes a single Observed
// record of the acknowledgement.
//
// The add is recorded as *acknowledged*, not resolved: Mem0's pipeline is
// asynchronous, so the EventAddRequested record states only that the request
// was accepted. Resolution is the reconciler's job (Task 19).
//
// The record carries record.EventAddRequested with an Observed
// ReasonAddAcknowledged reason whose evidence is the Mem0 response (event id
// and status), the Subject.ContentHash of the messages, and the joined message
// text as Content. The Subject.ContentHash length-prefixes each message
// individually, so the ambiguous "\n"-join in Content.Text cannot affect it.
//
// Two v1 limitations are deliberate and documented rather than plumbed through
// the signature:
//
//   - Add's signature carries no infer flag, so mem0.AddRequest.Infer is left
//     nil and Mem0's platform default applies. A caller needing an explicit
//     infer value must call mem0.Client directly.
//
//   - Add's signature carries no sensitivity input, so the record's
//     Content.Sensitive is always false. There is no way to express "this
//     memory is sensitive" through Add(messages []string); a caller that needs
//     it must classify and write through the ledger itself. This is a v1 gap.
//
//     Read that false carefully: it means UNCLASSIFIED, not verified
//     non-sensitive. Every record this method writes carries it, so a consumer
//     that trusts the flag and renders or exports without redaction will emit
//     this content in the clear. Treat records from this path as unclassified
//     until the caller supplies a sensitivity input.
//
// On a Mem0 error, Add returns that error and writes no record -- a Mem0
// failure is a real error, not an audit gap. When Mem0 succeeds, Add returns
// the response with a nil error unconditionally, even when the record cannot be
// written (see write).
func (m *Mem0Interceptor) Add(ctx context.Context, correlationID string, messages []string) (mem0.AddResponse, error) {
	if correlationID == "" {
		return mem0.AddResponse{}, ErrMissingCorrelationID
	}
	if m.mc == nil {
		return mem0.AddResponse{}, errors.New("library: mem0 client is not configured")
	}

	req := mem0.AddRequest{
		Messages: messagesToWire(messages),
		UserID:   m.scope.UserID,
		AgentID:  m.scope.AgentID,
		AppID:    m.scope.AppID,
		RunID:    m.scope.RunID,
	}
	resp, err := m.mc.Add(ctx, req)
	if err != nil {
		return mem0.AddResponse{}, err
	}

	m.observeAdd(correlationID, messages, resp)
	return resp, nil
}

// Search runs a Mem0 search and writes one Observed search_performed record
// plus one Observed memory_surfaced record per returned memory.
//
// The search_performed record is identified by the caller's correlation ID and
// carries the query, filters, ranking parameters, and the returned result
// count as its evidence. Each memory_surfaced record is identified by
// correlationID with a 1-based "#rank" suffix, carries the memory id in its
// Subject, the memory text as Content, and the memory's score and 1-based rank
// as its evidence.
//
// On a Mem0 error, Search returns that error and writes no record. When Mem0
// succeeds, Search returns the response with a nil error unconditionally, even
// when the records cannot be written (see write).
func (m *Mem0Interceptor) Search(ctx context.Context, correlationID string, q mem0.SearchRequest) (mem0.SearchResponse, error) {
	if correlationID == "" {
		return mem0.SearchResponse{}, ErrMissingCorrelationID
	}
	if m.mc == nil {
		return mem0.SearchResponse{}, errors.New("library: mem0 client is not configured")
	}

	resp, err := m.mc.Search(ctx, q)
	if err != nil {
		return mem0.SearchResponse{}, err
	}

	m.observeSearch(correlationID, q, resp)
	return resp, nil
}

// observeAdd builds and writes the single add_requested record. It is
// best-effort by construction: every step is total for the inputs Add can
// produce, and a step that cannot build a record simply skips the write rather
// than failing the caller's already-successful Add.
func (m *Mem0Interceptor) observeAdd(correlationID string, messages []string, resp mem0.AddResponse) {
	at := m.now().UTC()

	payload, err := json.Marshal(mem0.AddPayload{EventID: resp.EventID, Status: resp.Status})
	if err != nil {
		return
	}
	reason, ok := observedReason(record.ReasonAddAcknowledged, payload)
	if !ok {
		return
	}

	contentHash := mem0.ContentHash(messages...)
	rec := record.Record{
		ID:         record.RecordID(correlationID),
		At:         at,
		RecordedAt: at,
		Event:      record.EventAddRequested,
		Reason:     reason,
		Subject: record.Subject{
			Scope:       m.scope,
			ContentHash: contentHash,
		},
		Content: &record.Content{Text: strings.Join(messages, "\n")},
	}
	// The identifier is the caller's correlation ID (the record's own
	// identity), NOT resp.EventID. resp.EventID is Mem0's answer, and keying
	// on it would (a) make deduplication depend on the response, so a retry
	// Mem0 assigns a fresh event id to would not dedupe, and (b) collide two
	// genuinely distinct operations that happen to share a response id -- the
	// exact silent loss the key exists to prevent (see
	// TestAddContentHashCollisionResistance). The correlation ID identifies the
	// logical operation; Mem0's event id travels in the evidence payload.
	rec.IdempotencyKey = idemKey(record.EventAddRequested, record.ReasonAddAcknowledged, m.scope, "", correlationID, contentHash)
	m.write(rec)
}

// observeSearch builds and writes the search_performed record and then one
// memory_surfaced record per result, in rank order. Like observeAdd it is
// best-effort.
func (m *Mem0Interceptor) observeSearch(correlationID string, q mem0.SearchRequest, resp mem0.SearchResponse) {
	at := m.now().UTC()

	m.writeSearchPerformed(correlationID, q, len(resp.Results), at)
	for i, res := range resp.Results {
		m.writeSurfaced(correlationID, i+1, res, at)
	}
}

// writeSearchPerformed builds and writes the search_performed record. Its
// Content is deliberately nil: the query is already captured (canonically) in
// the Observed evidence payload, and the ledger should not carry a second,
// unsigned copy of caller-supplied query text as if it were memory content. The
// Subject.ContentHash still hashes the query, so the record's Subject is never
// the zero hash.
func (m *Mem0Interceptor) writeSearchPerformed(correlationID string, q mem0.SearchRequest, count int, at time.Time) {
	payload, err := json.Marshal(mem0.SearchPerformedPayload{
		Query:     q.Query,
		Filters:   q.Filters,
		TopK:      q.TopK,
		Threshold: q.Threshold,
		Rerank:    q.Rerank,
		Count:     count,
	})
	if err != nil {
		return
	}
	reason, ok := observedReason(record.ReasonSearchPerformed, payload)
	if !ok {
		return
	}

	contentHash := mem0.ContentHash(q.Query)
	rec := record.Record{
		ID:         record.RecordID(correlationID),
		At:         at,
		RecordedAt: at,
		Event:      record.EventSearchPerformed,
		Reason:     reason,
		Subject: record.Subject{
			Scope:       m.scope,
			ContentHash: contentHash,
		},
	}
	// A search_performed record has no Mem0 event id, so the correlation ID
	// carries the identifier for the key.
	rec.IdempotencyKey = idemKey(record.EventSearchPerformed, record.ReasonSearchPerformed, m.scope, "", correlationID, contentHash)
	m.write(rec)
}

// writeSurfaced builds and writes one memory_surfaced record for a single
// search result. rank is 1-based and supplies both the record ID's "#rank"
// suffix and the rank carried in the evidence payload.
func (m *Mem0Interceptor) writeSurfaced(correlationID string, rank int, res mem0.SearchResult, at time.Time) {
	payload, err := json.Marshal(mem0.MemorySurfacedPayload{Score: res.Score, Rank: rank})
	if err != nil {
		return
	}
	reason, ok := observedReason(record.ReasonReturnedBySearch, payload)
	if !ok {
		return
	}

	contentHash := mem0.ContentHash(res.Memory.Memory)
	derivedID := derivedRecordID(correlationID, rank)
	rec := record.Record{
		ID:         derivedID,
		At:         at,
		RecordedAt: at,
		Event:      record.EventMemorySurfaced,
		Reason:     reason,
		Subject: record.Subject{
			MemoryID:    res.ID,
			Scope:       m.scope,
			ContentHash: contentHash,
		},
		Content: &record.Content{Text: res.Memory.Memory},
	}
	// The key must identify THIS record, not merely the search it belongs to.
	// Every result of one search shares the correlation ID, and two results may
	// carry identical memory text -- hence an identical content hash -- so
	// keying on (correlationID, contentHash) would collapse them: Append would
	// treat the second as a duplicate, return the first record's ID with a nil
	// error, and write nothing -- the exact silent loss Task 18's Ruling R41
	// exists to prevent. The record's derived ID (correlationID plus its 1-based
	// "#rank" suffix) IS its identity and varies with rank even when the text
	// does not, so it is the identifier the key is built from: identical text at
	// different ranks derives different keys.
	//
	// The Mem0 memory ID (res.ID) travels on the record's Subject and is
	// deliberately NOT folded into the key: like an add's response event id, it
	// is response data, and keying on it would make deduplication depend on the
	// response (see the add_recorded reasoning in observeAdd) rather than on the
	// logical operation. The derived ID already distinguishes every record of a
	// single search, so it is sufficient and stable across a retry.
	rec.IdempotencyKey = idemKey(record.EventMemorySurfaced, record.ReasonReturnedBySearch, m.scope, "", string(derivedID), contentHash)
	m.write(rec)
}

// write hands rec to the audit writer. It is the fail-open boundary: the
// AuditWriter has already absorbed a ledger failure by recording a durable gap,
// so the only error it can return is the doubly-failed case (the ledger AND the
// gap log both refused the record).
//
// That error is deliberately discarded. When Mem0 has succeeded, the caller
// must get a nil error: turning a doubly-failed audit into a caller failure
// would be exactly the audit-problem-as-caller-problem that fail-open-loud
// exists to prevent. The loud channel (os.Stderr, by default) is the operator's
// signal, and the AuditWriter writes its marker to every channel before it
// returns that error, so the loud signal survives the discard.
//
// The discarded error is not lost in the sense that matters: a record that
// failed both the ledger and the gap log is reconciled on the Task 19
// background path, which reads the ledger directly and has no Mem0 call whose
// success a returned error could otherwise misreport. There, and only there,
// the error is actionable. This is the one place the error is intentionally
// dropped, so it is written as an explicit discard with this comment rather
// than an ignored return.
func (m *Mem0Interceptor) write(rec record.Record) {
	if m.aw == nil {
		return
	}
	_ = m.aw.Write(rec)
}

// idemKey derives the idempotency key for a record this interceptor writes,
// from the record's event, its reason kind (the tier distinguisher), the scope,
// the caller's correlation ID as the identifier, and the request digest.
//
// The identifier is the caller's correlation ID (eventID is empty): it
// identifies the logical operation and is stable across a retry, whereas the
// identifiers Mem0 returns are response data and would make deduplication
// depend on the response. A memory_surfaced record has no event id of its own,
// so it carries its DERIVED record ID (correlationID + "#" + rank) as the
// identifier instead -- the record's own identity, which distinguishes
// records even when two results of one search share identical text and so an
// identical digest.
//
// It is deliberately total. The caller's correlation ID is validated
// non-empty before any Mem0 call (ErrMissingCorrelationID), and DeriveIdemKey
// only fails when both the event id and the correlation id are empty, so for
// every call that reaches a builder DeriveIdemKey cannot fail. This branch is
// therefore unreachable in practice; it exists so that if it ever were
// reached, the record is still written -- keyless rather than dropped -- and
// the caller is never failed (Ruling A: a Mem0 success must return (resp,
// nil)). A keyless write appends normally; it simply is not deduplicatable.
func idemKey(event record.EventType, kind record.ReasonKind, scope record.Scope, eventID, correlationID string, digest record.Hash) record.IdemKey {
	key, err := record.DeriveIdemKey(event, kind, scope, eventID, correlationID, digest)
	if err != nil {
		return ""
	}
	return key
}

// observedReason builds an Observed reason of kind over raw JSON payload,
// reporting false when the payload or reason cannot be formed. Neither failure
// is reachable for the payloads this file builds, but the error is handled
// rather than panicked so no audit problem can crash a caller.
func observedReason(kind record.ReasonKind, payload []byte) (record.Reason, bool) {
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, payload)
	if err != nil {
		return record.Reason{}, false
	}
	reason, err := record.NewObservedReason(kind, ev)
	if err != nil {
		return record.Reason{}, false
	}
	return reason, true
}

// derivedRecordID returns the record ID for the rank-th derived record of a
// call: the correlation ID with a 1-based "#rank" suffix. The "#" separator
// cannot occur in a base64 correlation ID (see DeriveCorrelationID), so a
// derived ID can never be mistaken for a primary one.
func derivedRecordID(correlationID string, rank int) record.RecordID {
	return record.RecordID(fmt.Sprintf("%s#%d", correlationID, rank))
}

// messagesToWire maps Add's content strings onto Mem0 wire messages. Add's
// signature has no place for a role, so every message is sent as a user turn
// (defaultMessageRole).
func messagesToWire(messages []string) []mem0.Message {
	out := make([]mem0.Message, 0, len(messages))
	for _, msg := range messages {
		out = append(out, mem0.Message{Role: defaultMessageRole, Content: msg})
	}
	return out
}

// contentHashOf and its domain tag now live in internal/mem0 as
// mem0.ContentHash: the leaf package both this interceptor and the Phase 5
// reconciler already depend on, so the content-hash scheme has a single
// definition rather than two copies that could drift. See mem0.ContentHash and
// internal/mem0/content_test.go.

// DeriveCorrelationID derives a stable correlation ID for a caller with no
// natural one:
//
//	base64( sha256( "notary/corr/v1" ‖ L(UserID) ‖ L(AgentID) ‖ L(AppID)
//	                   ‖ L(RunID) ‖ L(seed) ) )
//
// where L(x) is a big-endian uint32 byte length followed by x's bytes. Each
// field is length-prefixed so that a scope/seed boundary cannot collide with a
// different split of the same bytes, and the domain tag separates the digest
// from every other SHA-256 use. The output is standard base64, which never
// contains "#", so a derived ID can never collide with the "#rank" suffix of a
// per-memory surfaced record.
//
// seed is an explicit argument and is never a timestamp: the scheme is
// deterministic in its inputs by construction, so it cannot silently become the
// clock-derived key the spec forbids.
func DeriveCorrelationID(scope record.Scope, seed string) string {
	h := sha256.New()
	h.Write([]byte(correlationDomain))
	var n [4]byte
	for _, f := range []string{scope.UserID, scope.AgentID, scope.AppID, scope.RunID, seed} {
		binary.BigEndian.PutUint32(n[:], uint32(len(f)))
		h.Write(n[:])
		h.Write([]byte(f))
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// The Observed evidence payloads that observeAdd, writeSearchPerformed and
// writeSurfaced marshal -- mem0.AddPayload, mem0.SearchPerformedPayload and
// mem0.MemorySurfacedPayload -- live in internal/mem0, beside the response
// types they project. They are shared with the Phase 5 reconciler, which reads
// the same payloads back and must not import its sibling interceptor. Their
// bytes are inside the canonical record hash, so their field NAMES and JSON
// TAGS are frozen; declaration order is not hashed (NewObservedEvidence
// canonicalises to sorted-key JSON), but mem0/evidence_test.go still pins it.
// internal/ledger's real-record fixtures are what guard this package's actual
// emitted bytes end to end.

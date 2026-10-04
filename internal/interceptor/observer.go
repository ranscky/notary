package interceptor

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"notary/internal/mem0"
	"notary/internal/record"
)

// correlationDomain domain-separates DeriveCorrelationID's digest from every
// other use of SHA-256, so its output can never be confused with a bare hash of
// the same concatenated fields.
const correlationDomain = "notary/corr/v1"

// Sink is everything the Observer needs to dispose of a record. The library
// hands it the AuditWriter (a synchronous append, absorbed by fail-open-loud);
// the proxy hands it its queue (a non-blocking enqueue). Neither knows which.
//
// *AuditWriter already satisfies it, so library mode passes the writer it
// already has. A nil Sink is safe: the Observer writes nothing.
type Sink interface {
	Write(rec record.Record) error
}

// Observer builds the Observed records for a Mem0 add and search and hands each
// to its Sink. It is the single implementation the library interceptor and the
// proxy share, so their records cannot drift.
//
// It holds no clock and no scope: both arrive per call, on AddObservation and
// SearchObservation, because the proxy's scope comes from each request's body
// and its two instants (arrival and record time) are captured at different
// points. The only state it retains is the immutable rule set, so its Add and
// Search are safe for concurrent use as long as the Sink is.
type Observer struct {
	// sink is where built records go. A nil sink is tolerated -- the record is
	// then simply not written -- matching library's tolerance of a nil
	// AuditWriter.
	sink Sink
	// rules is the sensitivity rule set consulted for every record, or nil for
	// none. It is set once, by NewObserver, and never mutated afterwards, so
	// the Observer stays safe for concurrent use. A nil rule set matches
	// nothing (RuleSet.Match is nil-safe).
	rules *RuleSet
}

// ObserverOption configures an Observer at construction.
type ObserverOption func(*Observer)

// WithRules configures the Observer to consult rs for every record it builds.
// A nil rs is valid and matches nothing, so it is the same as omitting the
// option and cannot make the Observer panic.
func WithRules(rs *RuleSet) ObserverOption {
	return func(o *Observer) { o.rules = rs }
}

// NewObserver returns an Observer that writes built records through sink and
// applies the construction options in opts.
//
// A nil sink is tolerated: the Observer builds records but writes none, so a
// caller without a ledger does not panic. A nil option is tolerated too, so a
// caller cannot crash construction by passing one. Like library.New, opts
// configure the Observer for its whole lifetime.
func NewObserver(sink Sink, opts ...ObserverOption) *Observer {
	o := &Observer{sink: sink}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	return o
}

// AddObservation is the input to Observer.Add: everything needed to build the
// single add_requested record for one Add. The scope and the two instants
// arrive here rather than at construction, because a proxy sources its scope,
// its arrival instant and its record instant from a single request.
//
// At is when the underlying Mem0 operation happened and RecordedAt is when
// Notary wrote the record; library mode passes the same instant for both (it
// reads its clock once, after the response), the proxy passes two.
type AddObservation struct {
	Scope         record.Scope
	CorrelationID string
	Messages      []string
	Metadata      map[string]any
	Response      mem0.AddResponse
	At            time.Time
	RecordedAt    time.Time
	Sensitive     bool
}

// SearchObservation is the input to Observer.Search: everything needed to build
// the search_performed record and one memory_surfaced record per result. As
// with AddObservation, the scope and the two instants arrive here rather than
// at construction.
type SearchObservation struct {
	Scope         record.Scope
	CorrelationID string
	Request       mem0.SearchRequest
	Response      mem0.SearchResponse
	At            time.Time
	RecordedAt    time.Time
	Sensitive     bool
}

// marking is one record's resolved content sensitivity, computed where the
// record's Content is built and before the record is hashed. sensitive is the
// OR of the caller's per-call flag and the Observer's rule set.
//
// It carries only the boolean. Which rule matched -- if any -- cannot be
// persisted: record.Content is {Text, Sensitive} and both are covered by the
// record hash, so recording the rule's name would change the digest of every
// existing record. The flag is the evidentiary fact; which rule produced it is
// declarative configuration, reproducible from the rules file.
type marking struct {
	sensitive bool
}

// classify resolves the sensitivity of one record's content. The caller's
// per-call flag and the Observer's rule set are an OR: either marks the content
// sensitive, and there is deliberately no way for a call flag to un-mark
// content a rule matched, because the safe direction is the only one worth
// having.
//
// metadata is the Mem0 metadata available where the record is built: the add
// request's metadata for the add path, and the per-result metadata Mem0
// returned for the search-surfaced path. A nil rule set matches nothing, so an
// Observer built without WithRules matches nothing (RuleSet.Match is nil-safe).
func (o *Observer) classify(callSensitive bool, scope record.Scope, metadata map[string]any) marking {
	_, matched := o.rules.Match(scope, metadata)
	return marking{sensitive: callSensitive || matched}
}

// Add builds the single add_requested record for obs and writes it. It is
// best-effort by construction: every step is total for the inputs an Add
// observation can carry, and a step that cannot build a record simply skips the
// write rather than failing the caller's already-successful Add.
//
// The record is the add *acknowledged*, not resolved: Mem0's pipeline is
// asynchronous, so the EventAddRequested record states only that the request
// was accepted. It carries an Observed ReasonAddAcknowledged reason whose
// evidence is the Mem0 response (event id and status), the
// Subject.ContentHash of the messages, and the joined message text as Content.
//
// metadata is the Mem0 metadata the add carried, which is what a sensitivity
// rule's metadata clause matches against.
func (o *Observer) Add(obs AddObservation) {
	payload, err := json.Marshal(mem0.AddPayload{EventID: obs.Response.EventID, Status: obs.Response.Status})
	if err != nil {
		return
	}
	reason, ok := observedReason(record.ReasonAddAcknowledged, payload)
	if !ok {
		return
	}

	contentHash := record.ContentHash(obs.Messages...)
	mark := o.classify(obs.Sensitive, obs.Scope, obs.Metadata)
	rec := record.Record{
		ID:         record.RecordID(obs.CorrelationID),
		At:         obs.At,
		RecordedAt: obs.RecordedAt,
		Event:      record.EventAddRequested,
		Reason:     reason,
		Subject: record.Subject{
			Scope:       obs.Scope,
			ContentHash: contentHash,
		},
		Content: &record.Content{Text: strings.Join(obs.Messages, "\n"), Sensitive: mark.sensitive},
	}
	// The identifier is the caller's correlation ID (the record's own
	// identity), NOT obs.Response.EventID. The response event id is Mem0's
	// answer, and keying on it would (a) make deduplication depend on the
	// response, so a retry Mem0 assigns a fresh event id to would not dedupe,
	// and (b) collide two genuinely distinct operations that happen to share a
	// response id -- the exact silent loss the key exists to prevent (see
	// TestAddContentHashCollisionResistance). The correlation ID identifies the
	// logical operation; Mem0's event id travels in the evidence payload.
	rec.IdempotencyKey = idemKey(record.EventAddRequested, record.ReasonAddAcknowledged, obs.Scope, "", obs.CorrelationID, contentHash)
	o.write(rec)
}

// Search builds and writes the search_performed record and then one
// memory_surfaced record per result, in rank order. Like Add it is
// best-effort. obs.Sensitive is the caller's per-call flag; each surfaced
// record's classification is that ORed with any matching rule, resolved where
// its Content is built.
func (o *Observer) Search(obs SearchObservation) {
	o.writeSearchPerformed(obs, len(obs.Response.Results))
	for i, res := range obs.Response.Results {
		o.writeSurfaced(obs, i+1, res)
	}
}

// writeSearchPerformed builds and writes the search_performed record. Its
// Content is deliberately nil: the query is already captured (canonically) in
// the Observed evidence payload, and the ledger should not carry a second,
// unsigned copy of caller-supplied query text as if it were memory content. The
// Subject.ContentHash still hashes the query, so the record's Subject is never
// the zero hash.
func (o *Observer) writeSearchPerformed(obs SearchObservation, count int) {
	payload, err := json.Marshal(mem0.SearchPerformedPayload{
		Query:     obs.Request.Query,
		Filters:   obs.Request.Filters,
		TopK:      obs.Request.TopK,
		Threshold: obs.Request.Threshold,
		Rerank:    obs.Request.Rerank,
		Count:     count,
	})
	if err != nil {
		return
	}
	reason, ok := observedReason(record.ReasonSearchPerformed, payload)
	if !ok {
		return
	}

	contentHash := record.ContentHash(obs.Request.Query)
	rec := record.Record{
		ID:         record.RecordID(obs.CorrelationID),
		At:         obs.At,
		RecordedAt: obs.RecordedAt,
		Event:      record.EventSearchPerformed,
		Reason:     reason,
		Subject: record.Subject{
			Scope:       obs.Scope,
			ContentHash: contentHash,
		},
	}
	// A search_performed record has no Mem0 event id, so the correlation ID
	// carries the identifier for the key.
	rec.IdempotencyKey = idemKey(record.EventSearchPerformed, record.ReasonSearchPerformed, obs.Scope, "", obs.CorrelationID, contentHash)
	o.write(rec)
}

// writeSurfaced builds and writes one memory_surfaced record for a single
// search result. rank is 1-based and supplies both the record ID's "#rank"
// suffix and the rank carried in the evidence payload. obs.Sensitive is the
// caller's per-call flag; the record's content is marked sensitive when that
// flag was set OR a rule matches the record's scope and the per-result metadata
// Mem0 returned (res.Memory.Metadata), resolved here before the record is
// hashed.
func (o *Observer) writeSurfaced(obs SearchObservation, rank int, res mem0.SearchResult) {
	payload, err := json.Marshal(mem0.MemorySurfacedPayload{Score: res.Score, Rank: rank})
	if err != nil {
		return
	}
	reason, ok := observedReason(record.ReasonReturnedBySearch, payload)
	if !ok {
		return
	}

	contentHash := record.ContentHash(res.Memory.Memory)
	derivedID := derivedRecordID(obs.CorrelationID, rank)
	mark := o.classify(obs.Sensitive, obs.Scope, res.Memory.Metadata)
	rec := record.Record{
		ID:         derivedID,
		At:         obs.At,
		RecordedAt: obs.RecordedAt,
		Event:      record.EventMemorySurfaced,
		Reason:     reason,
		Subject: record.Subject{
			MemoryID:    res.ID,
			Scope:       obs.Scope,
			ContentHash: contentHash,
		},
		Content: &record.Content{Text: res.Memory.Memory, Sensitive: mark.sensitive},
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
	// response (see the add_recorded reasoning in Add) rather than on the
	// logical operation. The derived ID already distinguishes every record of a
	// single search, so it is sufficient and stable across a retry.
	rec.IdempotencyKey = idemKey(record.EventMemorySurfaced, record.ReasonReturnedBySearch, obs.Scope, "", string(derivedID), contentHash)
	o.write(rec)
}

// write hands rec to the Sink, if any. A nil Sink is tolerated -- the record is
// simply not written.
//
// The Sink's error is deliberately discarded. When the caller has a
// fail-open-loud Sink (the AuditWriter), it has already absorbed a ledger
// failure by recording a durable gap, so the only error it can return is the
// doubly-failed case; turning that into a caller failure would be exactly the
// audit-problem-as-caller-problem that fail-open-loud exists to prevent. A
// different Sink (the proxy's queue) is likewise responsible for its own
// errors, which are reported on its own loud channel. This is the one place the
// error is intentionally dropped, so it is written as an explicit discard
// rather than an ignored return.
func (o *Observer) write(rec record.Record) {
	if o.sink == nil {
		return
	}
	_ = o.sink.Write(rec)
}

// idemKey derives the idempotency key for a record the Observer writes, from
// the record's event, its reason kind (the tier distinguisher), the scope, the
// caller's correlation ID as the identifier, and the request digest.
//
// The identifier is the caller's correlation ID (eventID is empty): it
// identifies the logical operation and is stable across a retry, whereas the
// identifiers Mem0 returns are response data and would make deduplication
// depend on the response. A memory_surfaced record has no event id of its own,
// so it carries its DERIVED record ID (correlationID + "#" + rank) as the
// identifier instead -- the record's own identity, which distinguishes records
// even when two results of one search share identical text and so an identical
// digest.
//
// It is deliberately total. The caller's correlation ID is validated non-empty
// before any Mem0 call (ErrMissingCorrelationID in library), and DeriveIdemKey
// only fails when both the event id and the correlation id are empty, so for
// every call that reaches a builder DeriveIdemKey cannot fail. This branch is
// therefore unreachable in practice; it exists so that if it ever were reached,
// the record is still written -- keyless rather than dropped -- and the caller
// is never failed (Ruling A: a Mem0 success must return (resp, nil)). A keyless
// write appends normally; it simply is not deduplicatable.
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
//
// It lives here, in the shared interceptor package, rather than in library:
// library re-exports it as a one-line delegator so its public surface is
// unchanged, but the definition has one home both the library interceptor and
// the proxy call.
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

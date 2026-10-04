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
//
// The records themselves are built by the shared interceptor.Observer, which
// the proxy (Phase 9) uses too, so the two deployment modes cannot drift. This
// package owns the Mem0 call, the correlation-ID and client validation, and the
// mapping of a call onto the Observer's per-call inputs.
package library

import (
	"context"
	"errors"
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
	// AuditWriter owns the gap log) but never the ledger. It is also the
	// Observer's Sink, so a nil aw means there is no ledger to write to.
	aw *interceptor.AuditWriter
	// scope is the caller context stamped on every record this interceptor
	// writes.
	scope record.Scope
	// now supplies the clock used to stamp a record's At and RecordedAt. It is
	// never time.Now in the write path unless the caller supplied nil.
	now func() time.Time
	// rules is the sensitivity rule set the construction options set, or nil
	// for none. It is read once, by New, to build observer, and never mutated
	// afterwards, so the interceptor stays safe for concurrent use (see the
	// type comment). A nil rule set matches nothing.
	rules *interceptor.RuleSet
	// observer builds every record this interceptor writes. It is built in New
	// from aw and rules and holds no mutable state, so Add and Search stay safe
	// for concurrent use (see the type comment).
	observer *interceptor.Observer
}

// New returns a Mem0Interceptor that calls mc, writes through aw, stamps its
// records with scope, reads its clock from now, and applies the construction
// options in opts.
//
// A nil now falls back to time.Now, mirroring ledger.New: an unset clock must
// not panic, and it must not leave a record stamped with the zero time (which
// would make it invisible to a time-window read). A nil aw is tolerated -- the
// record is then simply not written, since there is no ledger to write it to.
// A nil option is tolerated too, so a caller cannot crash construction by
// passing one.
//
// opts configure the interceptor for its whole lifetime, as distinct from the
// per-call CallOptions Add and Search accept. WithSensitivityRules is the
// option this package defines; with no option the interceptor consults no rules
// and every call's classification comes solely from its own CallOptions.
//
// aw is borrowed. Close closes it, because the AuditWriter owns the gap log's
// lifetime; the ledger's lifetime belongs to the caller that built the
// AuditWriter.
func New(mc *mem0.Client, aw *interceptor.AuditWriter, scope record.Scope, now func() time.Time, opts ...Option) *Mem0Interceptor {
	if now == nil {
		now = time.Now
	}
	m := &Mem0Interceptor{mc: mc, aw: aw, scope: scope, now: now}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	// The Observer shares this interceptor's writer and rule set. Its records
	// are byte-identical to what this package built inline before the
	// extraction. The nil-aw guard lives at the call sites (Add and Search):
	// a nil *interceptor.AuditWriter stored in the Sink interface is not a nil
	// interface, so handing it the Observer to write would call Write on a nil
	// pointer and panic.
	m.observer = interceptor.NewObserver(aw, interceptor.WithRules(m.rules))
	return m
}

// callConfig is the resolved per-call configuration Add and Search derive from
// their variadic CallOption arguments. Its zero value is the documented
// default: a call that supplies no option records content that is unclassified,
// and its records carry Sensitive false.
type callConfig struct {
	// sensitive marks the content this one call records as sensitive.
	sensitive bool
}

// CallOption customises a single Add or Search call. An option is a value, not
// state on the interceptor: it applies only to the call it is passed to and is
// never retained, so one call's classification cannot leak into another and
// the interceptor stays safe for concurrent use.
type CallOption func(*callConfig)

// Sensitive returns a CallOption that marks the content the call records as
// sensitive. It sets Content.Sensitive on every Observed record the call writes
// -- the add_requested record, or each memory_surfaced record -- before that
// record is hashed, so the classification is tamper-evident (see
// TestSensitiveChangesTheHash). A search_performed record carries no content, so
// there is nothing for the option to mark on it.
//
// Omitting the option leaves the content unclassified rather than verified
// non-sensitive; read Add's doc comment for what that distinction requires of a
// consumer.
func Sensitive() CallOption {
	return func(c *callConfig) { c.sensitive = true }
}

// resolveCallOptions folds a call's options onto its default configuration. It
// tolerates a nil option so a caller cannot crash the interceptor by passing
// one.
func resolveCallOptions(opts []CallOption) callConfig {
	var cfg callConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
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
// One v1 limitation is deliberate and documented rather than plumbed through
// the signature:
//
//   - Add's signature carries no infer flag, so mem0.AddRequest.Infer is left
//     nil and Mem0's platform default applies. A caller needing an explicit
//     infer value must call mem0.Client directly.
//
// Sensitivity, by contrast, is expressible per call: passing Sensitive() marks
// the content this Add records as sensitive (Content.Sensitive true). The flag
// is set before the record is hashed, so the classification is tamper-evident.
// A rule configured at construction with WithSensitivityRules marks it too,
// where the rule matches the interceptor's scope and the add's metadata; the
// two are an OR, so either marks it. With neither, the content is UNCLASSIFIED,
// not verified non-sensitive: a consumer that trusts the flag and renders or
// exports without redaction will emit content it was never told to protect.
// Treat an unclassified record as content to redact until something -- this
// option, or a matching rule -- says otherwise.
//
// On a Mem0 error, Add returns that error and writes no record -- a Mem0
// failure is a real error, not an audit gap. When Mem0 succeeds, Add returns
// the response with a nil error unconditionally, even when the record cannot be
// written (see interceptor.Observer).
func (m *Mem0Interceptor) Add(ctx context.Context, correlationID string, messages []string, opts ...CallOption) (mem0.AddResponse, error) {
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

	// Keep library's tolerance of a nil AuditWriter. A nil *AuditWriter stored
	// in the Observer's Sink interface is a non-nil interface, so it would call
	// Write on a nil pointer and panic; the guard must live here, before the
	// Observer call, rather than behind the interface.
	if m.aw == nil {
		return resp, nil
	}
	at := m.now().UTC()
	m.observer.Add(interceptor.AddObservation{
		Scope:         m.scope,
		CorrelationID: correlationID,
		Messages:      messages,
		Metadata:      req.Metadata,
		Response:      resp,
		At:            at,
		RecordedAt:    at,
		Sensitive:     resolveCallOptions(opts).sensitive,
	})
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
// Passing Sensitive() marks each memory_surfaced record's content sensitive,
// exactly as it does for Add; so does a matching rule configured with
// WithSensitivityRules, matched against the interceptor's scope and the
// per-result metadata Mem0 returned. The search_performed record carries no
// content, so neither path touches it.
//
// On a Mem0 error, Search returns that error and writes no record. When Mem0
// succeeds, Search returns the response with a nil error unconditionally, even
// when the records cannot be written (see interceptor.Observer).
func (m *Mem0Interceptor) Search(ctx context.Context, correlationID string, q mem0.SearchRequest, opts ...CallOption) (mem0.SearchResponse, error) {
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

	// The same nil-AuditWriter guard as Add, for the same reason: a nil
	// *AuditWriter in the Sink interface is not a nil interface.
	if m.aw == nil {
		return resp, nil
	}
	at := m.now().UTC()
	m.observer.Search(interceptor.SearchObservation{
		Scope:         m.scope,
		CorrelationID: correlationID,
		Request:       q,
		Response:      resp,
		At:            at,
		RecordedAt:    at,
		Sensitive:     resolveCallOptions(opts).sensitive,
	})
	return resp, nil
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

// The content-hash scheme now lives in internal/record as record.ContentHash:
// beside Subject.ContentHash, the field it fills, and alongside record's other
// domain-separated hashing. That gives the scheme a single definition rather
// than two copies that could drift. See record.ContentHash and
// internal/record/content_test.go.

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
// The definition lives in internal/interceptor, where the shared Observer is,
// so the library interceptor and the proxy derive identical IDs from one
// implementation. This is a one-line delegator that keeps this package's
// exported surface intact.
func DeriveCorrelationID(scope record.Scope, seed string) string {
	return interceptor.DeriveCorrelationID(scope, seed)
}

// The Observed evidence payloads the Observer marshals -- mem0.AddPayload,
// mem0.SearchPerformedPayload and mem0.MemorySurfacedPayload -- live in
// internal/mem0, beside the response types they project. They are shared with
// the Phase 5 reconciler, which reads the same payloads back and must not
// import its sibling interceptor. Their bytes are inside the canonical record
// hash, so their field NAMES and JSON TAGS are frozen; declaration order is not
// hashed (NewObservedEvidence canonicalises to sorted-key JSON), but
// mem0/evidence_test.go still pins it. internal/ledger's real-record fixtures
// are what guard this package's actual emitted bytes end to end.

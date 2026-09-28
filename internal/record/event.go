package record

import (
	"fmt"
	"time"
)

// EventType names the kind of event a Record describes. Along with Reason, it
// is what makes an audit record a statement rather than a bare timestamp: it
// says what happened, and Reason says why.
type EventType string

const (
	// EventAddRequested records that Notary asked Mem0 to add a memory.
	EventAddRequested EventType = "add_requested"
	// EventAddResolved records that an add request reached a terminal outcome.
	EventAddResolved EventType = "add_resolved"
	// EventSearchPerformed records that Notary issued a search to Mem0.
	EventSearchPerformed EventType = "search_performed"
	// EventMemorySurfaced records that a memory was returned by a search.
	EventMemorySurfaced EventType = "memory_surfaced"
	// EventMemoryKept records that a surfaced memory was kept.
	EventMemoryKept EventType = "memory_kept"
	// EventMemoryDropped records that a surfaced memory was dropped.
	EventMemoryDropped EventType = "memory_dropped"
	// EventAuditGap records that a span of Mem0 activity could not be audited.
	EventAuditGap EventType = "audit_gap"
)

// allEventTypes is the closed vocabulary of event types. It is populated at
// package initialisation and never mutated afterwards, so it is safe to read
// concurrently from Valid.
var allEventTypes = map[EventType]struct{}{
	EventAddRequested:    {},
	EventAddResolved:     {},
	EventSearchPerformed: {},
	EventMemorySurfaced:  {},
	EventMemoryKept:      {},
	EventMemoryDropped:   {},
	EventAuditGap:        {},
}

// Valid reports whether et is one of the defined event types. It returns false
// for the zero value and for any unknown value, so a defaulted or misspelled
// event can never be mistaken for a real one.
func (et EventType) Valid() bool {
	_, ok := allEventTypes[et]
	return ok
}

// String returns the event's wire name, which is already lowercase
// snake_case. An unknown value is returned unchanged rather than being mapped
// to a placeholder label, so String never fabricates a name that Valid would
// reject.
func (et EventType) String() string { return string(et) }

// Scope identifies the caller context a memory belongs to: the user, agent,
// application, and run the event was part of. Empty fields are allowed, since
// not every caller supplies every dimension.
type Scope struct {
	UserID  string
	AgentID string
	AppID   string
	RunID   string
}

// Subject identifies the memory an event concerns: its Mem0 memory ID, the
// scope it lives in, and the hash of its content. MemoryID may be empty for
// events that precede a memory's existence, such as add_requested, which is
// why Validate does not require it.
type Subject struct {
	MemoryID    string
	Scope       Scope
	ContentHash Hash
}

// Content is the memory text a record may carry, together with whether that
// text was classified as sensitive. A record may carry no content at all (a
// nil *Content), and non-nil content may still have empty Text; the content
// hash on the Subject is what matters for integrity.
type Content struct {
	Text      string
	Sensitive bool
}

// Record is the unit that Notary hash-chains and signs: the immutable,
// authoritative statement of why an agent memory was kept, dropped, or
// surfaced. The whole audit trail is a sequence of these.
//
// The field names and types are a wire contract: later tasks hash the record
// (Task 5), serialise it (Task 6), and assign Seq, PrevHash, Hash, and
// Signature (Task 9). There are deliberately no struct tags here, because the
// canonical encoding is defined explicitly by Task 5 and persistence by
// Task 6; a second, tag-driven serialisation would compete with those.
type Record struct {
	// ID is the stable identifier of this record.
	ID RecordID
	// Seq is this record's position in the ledger. It is assigned by
	// Ledger.Append and is not checked by Validate.
	Seq uint64
	// At is when the underlying Mem0 event happened.
	At time.Time
	// RecordedAt is when Notary wrote this record.
	RecordedAt time.Time
	// Event names what happened.
	Event EventType
	// Reason is the structured "why" behind the event.
	Reason Reason
	// Subject identifies the memory the event concerns.
	Subject Subject
	// Content is the memory text, when the record carries any; it may be nil.
	Content *Content
	// IdempotencyKey deduplicates repeated writes of the same event.
	IdempotencyKey IdemKey
	// PrevHash links this record to its predecessor in the chain. It is
	// assigned by Ledger.Append and is not checked by Validate.
	PrevHash Hash
	// Hash is this record's own digest. It is assigned by Ledger.Append and
	// is not checked by Validate.
	Hash Hash
	// Signature signs this record. It is assigned by Ledger.Append and is not
	// checked by Validate.
	Signature []byte
	// SignerKeyID names the key that produced Signature.
	SignerKeyID string
}

// Validate reports whether r is well formed as a claim: it has an ID, a known
// EventType, a valid Reason, and a non-zero Subject.ContentHash. It returns a
// non-nil error otherwise, naming the field that failed.
//
// Validate deliberately does NOT check Seq, Hash, PrevHash, or Signature.
// Those fields are assigned later by Ledger.Append (Task 9), after a record
// has been fully built but before it is appended. Rejecting a zero Seq here
// would make the genesis record -- the first record in the chain, whose Seq is
// legitimately 0 and whose PrevHash is legitimately all-zero -- impossible to
// construct. Do not "fix" Validate by adding those checks; they belong to the
// append path.
//
// Content may be nil, for an event that carries no memory text, and
// non-nil Content may have empty Text; the content hash on Subject is what
// matters.
func (r Record) Validate() error {
	if r.ID == "" {
		return fmt.Errorf("record: record has an empty ID")
	}
	if !r.Event.Valid() {
		return fmt.Errorf("record: record %s has unknown event type %q", r.ID, r.Event)
	}
	if err := r.Reason.Validate(); err != nil {
		return fmt.Errorf("record: record %s has an invalid reason: %w", r.ID, err)
	}
	if r.Subject.ContentHash == (Hash{}) {
		return fmt.Errorf("record: record %s has an all-zero subject content hash", r.ID)
	}
	return nil
}

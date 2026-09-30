package mem0

// The three types below are the Observed evidence payloads the in-process
// interceptor writes for a Mem0 add and search: the add acknowledgement, the
// search that was performed, and one memory surfacing in that search's
// results.
//
// They live here, beside the response types they project, because two peers
// need them: internal/interceptor/library writes them, and the Phase 5
// reconciler (internal/reconcile) reads them back. The reconciler must not
// import its sibling interceptor, and re-declaring the JSON shapes in
// reconcile would let the two drift silently, so the shared, exported
// declaration lives in this leaf package both already depend on.
//
// CRITICAL: these are hashed, wire-frozen shapes.
//
// Each payload is embedded in ObservedEvidence.Payload, which Reason.Encode
// renders and CanonicalBytes writes straight into the canonical record hash.
// The encoded bytes of these payloads are therefore part of every
// add_requested, search_performed and memory_surfaced record already signed
// against real ledgers. encoding/json emits struct fields in declaration
// order, so a field's ORDER is hashed as much as its name: reordering a field,
// renaming one, or changing a JSON tag would make every historical record of
// that kind fail verify. Field order, names and tags must be preserved exactly
// as declared; internal/interceptor/library/evidence_golden_test.go and
// internal/mem0/evidence_test.go pin them.

// AddPayload is the Observed evidence payload of an add_requested record: the
// acknowledgement Mem0 returned. It carries only the event id and status,
// which are exactly what the add response reported.
type AddPayload struct {
	EventID string `json:"event_id"`
	Status  string `json:"status"`
}

// SearchPerformedPayload is the Observed evidence payload of a
// search_performed record: what was asked and how much came back.
type SearchPerformedPayload struct {
	Query     string  `json:"query"`
	Filters   Filters `json:"filters"`
	TopK      int     `json:"top_k"`
	Threshold float64 `json:"threshold"`
	Rerank    bool    `json:"rerank"`
	Count     int     `json:"count"`
}

// MemorySurfacedPayload is the Observed evidence payload of a
// memory_surfaced record: the ranking evidence that caused the memory to
// surface.
type MemorySurfacedPayload struct {
	Score float64 `json:"score"`
	Rank  int     `json:"rank"`
}

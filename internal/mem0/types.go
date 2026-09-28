// Package mem0 is a thin REST client for the hosted Mem0 platform API.
//
// The client is deliberately a faithful decoder: every response field the
// recorded fixtures prove is modelled, because Notary's audit trail must be
// able to distinguish "Mem0 reported this" from "Notary inferred it". A client
// that quietly drops a response field destroys evidence an auditor needs.
//
// The wire shapes here are pinned to the seven real recorded responses in
// testdata/ (see testdata/FIXTURES.md). That file, not the API documentation,
// is the authority on field presence, because field presence is inconsistent
// between endpoints: search emits agent_id/app_id/run_id as explicit null
// while get_all omits them entirely, and metadata is {} in one and null in the
// other. Optional fields are therefore pointer- or map-typed so that an
// explicit null and an absent field both decode without error.
package mem0

// Message is one chat message, used both as Add input and as the echoed input
// on a history record.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Filters carries the entity scope for Search and GetAll.
//
// Entity IDs MUST live here, nested inside each request's "filters" object:
// Mem0 answers a top-level entity id with HTTP 400. Expressing the scope as
// fields of Filters — and giving SearchRequest and GetAllRequest no top-level
// entity-id field at all — makes the rejected shape unrepresentable at a call
// site, rather than a rule every caller has to remember.
type Filters struct {
	UserID  string `json:"user_id,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	AppID   string `json:"app_id,omitempty"`
	RunID   string `json:"run_id,omitempty"`
}

// Memory is the memory object returned by the get_all (list) and search
// endpoints.
//
// Fields the endpoints disagree about are pointers: search emits
// agent_id/app_id/run_id as explicit null and get_all omits them entirely, so
// a missing field and an explicit null both decode to a nil pointer. Metadata
// is null in get_all and {} in search, both of which decode to a map.
type Memory struct {
	ID         string         `json:"id"`
	Memory     string         `json:"memory"`
	UserID     string         `json:"user_id"`
	AgentID    *string        `json:"agent_id"`
	AppID      *string        `json:"app_id"`
	RunID      *string        `json:"run_id"`
	Metadata   map[string]any `json:"metadata"`
	Categories []string       `json:"categories"`
	CreatedAt  string         `json:"created_at"`
	UpdatedAt  string         `json:"updated_at"`
	ExpiresAt  *string        `json:"expiration_date"`

	// StructuredAttributes is a derived decomposition of the memory's
	// timestamp (year, month, day, day_of_week, ...). It is modelled as a raw
	// map so that a key Mem0 adds later survives decoding instead of being
	// silently dropped by a fixed struct.
	StructuredAttributes map[string]any `json:"structured_attributes"`
	// ReplacedBy is the id of the memory that superseded this one, or nil when
	// this memory is current. Only get_all returns it; it reveals supersession.
	ReplacedBy *string `json:"replaced_by"`
	// Synthesized reports that the platform generated the content.
	Synthesized bool `json:"synthesized"`

	// Hash is retained for completeness only: no recorded fixture shows a
	// "hash" field on any endpoint, so nothing here proves the platform
	// returns it.
	Hash string `json:"hash"`
}

// SearchResult is a Memory plus the ranking evidence that surfaced it. The
// embedded Memory's fields are promoted into the JSON object, matching the
// search response.
//
// Note the embedded type is named Memory and so is its Memory field, so the
// promoted selector r.Memory resolves to the embedded struct: the memory text
// is reached as r.Memory.Memory. Every other promoted field (r.ID, r.UserID, …)
// reads directly.
type SearchResult struct {
	Memory
	Score          float64        `json:"score"`
	ScoreBreakdown ScoreBreakdown `json:"score_breakdown"`
}

// ScoreBreakdown is the per-signal decomposition that accompanies a search
// score. It is stronger evidence for "why was this surfaced" than score alone.
type ScoreBreakdown struct {
	Semantic float64 `json:"semantic"`
	BM25     float64 `json:"bm25"`
	Entity   float64 `json:"entity"`
}

// AddRequest is the body of POST /v3/memories/add/. Unlike Search and GetAll,
// Add takes its scope fields at the top level, matching the recorded request.
type AddRequest struct {
	Messages []Message      `json:"messages"`
	UserID   string         `json:"user_id,omitempty"`
	AgentID  string         `json:"agent_id,omitempty"`
	AppID    string         `json:"app_id,omitempty"`
	RunID    string         `json:"run_id,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// Infer asks Mem0 to extract memories from the messages. It is a pointer so
	// that nil omits the field (use the platform default) and is distinct from
	// an explicit false.
	Infer *bool `json:"infer,omitempty"`
}

// AddResponse is the acknowledgement of an Add. The pipeline is asynchronous,
// so this confirms acceptance only; the outcome is fetched later with
// EventStatus.
type AddResponse struct {
	EventID string `json:"event_id"`
	Status  string `json:"status"`
	// Message is carried by some responses; the recorded add response omits it,
	// so it is normally empty. It is kept so a message-bearing body is not
	// silently dropped.
	Message string `json:"message"`
}

// SearchRequest is the body of POST /v3/memories/search/. The entity scope
// lives in Filters (see Filters); there is deliberately no top-level entity-id
// field.
type SearchRequest struct {
	Query     string  `json:"query"`
	Filters   Filters `json:"filters"`
	TopK      int     `json:"top_k,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
	Rerank    bool    `json:"rerank,omitempty"`
}

// SearchResponse is the envelope returned by Search.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
}

// GetAllRequest is the body of POST /v3/memories/. As with SearchRequest, the
// entity scope lives in Filters and nowhere else.
type GetAllRequest struct {
	Filters Filters `json:"filters"`
}

// GetAllResponse is the paginated envelope returned by GetAll. Next and
// Previous are opaque cursors, null on the first and last page.
type GetAllResponse struct {
	Count    int      `json:"count"`
	Next     *string  `json:"next"`
	Previous *string  `json:"previous"`
	Results  []Memory `json:"results"`
}

// HistoryEvent is one entry in a memory's change log. OldMemory and NewMemory
// are pointers because an ADD has a nil old_memory; the pair distinguishes
// creation from modification.
type HistoryEvent struct {
	ID         string         `json:"id"`
	MemoryID   string         `json:"memory_id"`
	Input      []Message      `json:"input"`
	OldMemory  *string        `json:"old_memory"`
	NewMemory  *string        `json:"new_memory"`
	UserID     string         `json:"user_id"`
	Categories []string       `json:"categories"`
	Metadata   map[string]any `json:"metadata"`
	CreatedAt  string         `json:"created_at"`
	UpdatedAt  string         `json:"updated_at"`
	Event      string         `json:"event"`
}

// HistoryResponse is the change-log list returned by History.
type HistoryResponse []HistoryEvent

// EventStatusResponse is the full asynchronous event record returned by
// EventStatus. It is far richer than a status: it echoes the original request
// payload, reports the per-memory results, and carries the pipeline's own
// timing and source.
type EventStatusResponse struct {
	ID          string         `json:"id"`
	EventType   string         `json:"event_type"`
	Status      string         `json:"status"`
	Payload     map[string]any `json:"payload"`
	Metadata    map[string]any `json:"metadata"`
	Results     []EventResult  `json:"results"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
	StartedAt   string         `json:"started_at"`
	CompletedAt string         `json:"completed_at"`
	Source      string         `json:"source"`
	Latency     float64        `json:"latency"`
	GraphStatus *string        `json:"graph_status"`
	Error       *string        `json:"error"`
}

// EventResult is one memory-level outcome of an event. Event is the action
// applied ("ADD") and ID is the resulting memory id.
type EventResult struct {
	EventStart   *string        `json:"event_start"`
	EventEnd     *string        `json:"event_end"`
	Data         map[string]any `json:"data"`
	UserID       string         `json:"user_id"`
	AttributedTo string         `json:"attributed_to"`
	EventDate    *string        `json:"event_date"`
	MemoryType   *string        `json:"memory_type"`
	ID           string         `json:"id"`
	Event        string         `json:"event"`
	PlanStatus   *string        `json:"plan_status"`
}

package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// ReasonKind names the structured "why" carried by a Reason: the specific
// event or decision that produced an audit record.
//
// A kind is not free-form. Each kind belongs to exactly one VisibilityTier,
// reported by AllowedTier, and the per-tier constructors refuse a kind whose
// allowed tier is not their own. That single-valued mapping is what keeps an
// "observed" claim from ever being emitted through the path that emits
// "reconstructed" claims.
type ReasonKind string

const (
	// ReasonSearchPerformed records that Notary issued a search request to Mem0.
	ReasonSearchPerformed ReasonKind = "search_performed"
	// ReasonAddAcknowledged records that Mem0 acknowledged an add request.
	ReasonAddAcknowledged ReasonKind = "add_acknowledged"
	// ReasonReturnedBySearch records that a memory was returned by a search.
	ReasonReturnedBySearch ReasonKind = "returned_by_search"
	// ReasonStoredByMem0 records that Mem0 stored a memory.
	ReasonStoredByMem0 ReasonKind = "stored_by_mem0"
	// ReasonKeptByContentMatch records that a memory was kept because its
	// content matched an earlier record.
	ReasonKeptByContentMatch ReasonKind = "kept_by_content_match"
	// ReasonAbsentFromSearch records that a memory expected to surface did not
	// appear in a search's results.
	ReasonAbsentFromSearch ReasonKind = "absent_from_search"
	// ReasonNoFactsExtracted records that no facts could be extracted from an
	// interaction.
	ReasonNoFactsExtracted ReasonKind = "no_facts_extracted"
	// ReasonRemovedByMem0 records that Mem0 removed a memory.
	ReasonRemovedByMem0 ReasonKind = "removed_by_mem0"
	// ReasonAddFailed records that an add request to Mem0 failed.
	ReasonAddFailed ReasonKind = "add_failed"
	// ReasonAuditUnavailable records that the audit trail for Mem0 could not be
	// read.
	ReasonAuditUnavailable ReasonKind = "audit_unavailable"
)

// AllowedTier returns the single VisibilityTier a kind may carry. The second
// result is false for any kind not in the vocabulary above, so an unknown kind
// can never be mistaken for a known one.
func (k ReasonKind) AllowedTier() (VisibilityTier, bool) {
	switch k {
	case ReasonSearchPerformed,
		ReasonAddAcknowledged,
		ReasonReturnedBySearch,
		ReasonStoredByMem0,
		ReasonAddFailed,
		ReasonAuditUnavailable:
		return Observed, true
	case ReasonKeptByContentMatch,
		ReasonAbsentFromSearch,
		ReasonNoFactsExtracted:
		return Reconstructed, true
	case ReasonRemovedByMem0:
		return Internal, true
	default:
		return VisibilityTier{}, false
	}
}

// EvidenceSource names where observed evidence came from. The zero value is
// deliberately invalid, so an unintentionally defaulted source is detectable.
type EvidenceSource uint8

const (
	// SourceInvalid is the zero value of EvidenceSource and is never valid.
	SourceInvalid EvidenceSource = iota
	// SourceMem0Response marks evidence captured from a Mem0 API response.
	SourceMem0Response
	// SourceNotaryInstrumentation marks evidence captured by Notary's own
	// instrumentation around a Mem0 call.
	SourceNotaryInstrumentation
)

// evidenceSourceName returns the lowercase wire name of a source. Any
// unrecognised source maps to "invalid".
func evidenceSourceName(src EvidenceSource) string {
	switch src {
	case SourceMem0Response:
		return "mem0_response"
	case SourceNotaryInstrumentation:
		return "notary_instrumentation"
	default:
		return "invalid"
	}
}

// parseEvidenceSource reverses evidenceSourceName. It reports an error for any
// name that is not a known source, including "invalid".
func parseEvidenceSource(name string) (EvidenceSource, error) {
	switch name {
	case "mem0_response":
		return SourceMem0Response, nil
	case "notary_instrumentation":
		return SourceNotaryInstrumentation, nil
	default:
		return SourceInvalid, fmt.Errorf("record: unknown evidence source %q", name)
	}
}

// validate reports whether src is a known, valid source.
func (src EvidenceSource) validate() error {
	switch src {
	case SourceMem0Response, SourceNotaryInstrumentation:
		return nil
	default:
		return fmt.Errorf("record: invalid evidence source %d", uint8(src))
	}
}

// ObservedEvidence is a payload captured directly from an interaction, a Mem0
// response, or Notary's own instrumentation. Its fields are unexported, so an
// ObservedEvidence can only come from NewObservedEvidence (or the invalid zero
// value), and its payload is always canonical JSON.
type ObservedEvidence struct {
	source  EvidenceSource
	payload []byte
}

// NewObservedEvidence builds an ObservedEvidence from a source and a JSON
// payload, rejecting an invalid source and any payload that is not a single
// JSON value.
//
// The payload is canonicalised: it is decoded with json.Number semantics and
// re-marshalled, so insignificant key order and whitespace, and the literal
// form of numbers, cannot perturb a downstream record hash. Two payloads that
// differ only in those ways therefore produce byte-identical evidence.
func NewObservedEvidence(src EvidenceSource, payload []byte) (ObservedEvidence, error) {
	if err := src.validate(); err != nil {
		return ObservedEvidence{}, err
	}

	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return ObservedEvidence{}, fmt.Errorf("record: observed payload is not valid JSON: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return ObservedEvidence{}, fmt.Errorf("record: observed payload has trailing JSON content")
	}

	canon, err := json.Marshal(v)
	if err != nil {
		return ObservedEvidence{}, fmt.Errorf("record: canonicalise observed payload: %w", err)
	}
	return ObservedEvidence{source: src, payload: canon}, nil
}

// clone returns a deep copy of ev so a caller cannot mutate evidence held
// inside a Reason.
func (ev ObservedEvidence) clone() ObservedEvidence {
	return ObservedEvidence{source: ev.source, payload: append([]byte(nil), ev.payload...)}
}

// validate reports whether ev is a well-formed observed evidence value.
func (ev ObservedEvidence) validate() error {
	if err := ev.source.validate(); err != nil {
		return err
	}
	if len(ev.payload) == 0 {
		return fmt.Errorf("record: observed evidence has an empty payload")
	}
	return nil
}

// ReconstructedEvidence is evidence inferred or rebuilt from other records:
// the record IDs it rests on, the rule and rule version that produced it, and
// the confidence assigned to the inference. Its fields are unexported, so it
// can only come from NewReconstructedEvidence (or the invalid zero value).
type ReconstructedEvidence struct {
	basis       []RecordID
	rule        string
	ruleVersion string
	confidence  float64
}

// NewReconstructedEvidence builds a ReconstructedEvidence. It rejects an empty
// basis (an inference with nothing to rest on is not evidence), an empty rule,
// or an empty rule version.
func NewReconstructedEvidence(basis []RecordID, rule, ruleVersion string, confidence float64) (ReconstructedEvidence, error) {
	if len(basis) == 0 {
		return ReconstructedEvidence{}, fmt.Errorf("record: reconstructed evidence requires a non-empty basis")
	}
	if rule == "" {
		return ReconstructedEvidence{}, fmt.Errorf("record: reconstructed evidence requires a rule")
	}
	if ruleVersion == "" {
		return ReconstructedEvidence{}, fmt.Errorf("record: reconstructed evidence requires a rule version")
	}
	return ReconstructedEvidence{
		basis:       append([]RecordID(nil), basis...),
		rule:        rule,
		ruleVersion: ruleVersion,
		confidence:  confidence,
	}, nil
}

// clone returns a deep copy of ev so a caller cannot mutate evidence held
// inside a Reason.
func (ev ReconstructedEvidence) clone() ReconstructedEvidence {
	return ReconstructedEvidence{
		basis:       append([]RecordID(nil), ev.basis...),
		rule:        ev.rule,
		ruleVersion: ev.ruleVersion,
		confidence:  ev.confidence,
	}
}

// validate reports whether ev is a well-formed reconstructed evidence value.
func (ev ReconstructedEvidence) validate() error {
	if len(ev.basis) == 0 {
		return fmt.Errorf("record: reconstructed evidence requires a non-empty basis")
	}
	if ev.rule == "" {
		return fmt.Errorf("record: reconstructed evidence requires a rule")
	}
	if ev.ruleVersion == "" {
		return fmt.Errorf("record: reconstructed evidence requires a rule version")
	}
	return nil
}

// InternalNote is an opaque note produced by Notary itself, carrying no
// observable claim. Its field is unexported, so it can only come from
// NewInternalNote (or the invalid zero value).
type InternalNote struct {
	opaque string
}

// NewInternalNote builds an InternalNote, rejecting an empty string.
func NewInternalNote(opaque string) (InternalNote, error) {
	if opaque == "" {
		return InternalNote{}, fmt.Errorf("record: internal note requires a non-empty string")
	}
	return InternalNote{opaque: opaque}, nil
}

// validate reports whether note is a well-formed internal note.
func (note InternalNote) validate() error {
	if note.opaque == "" {
		return fmt.Errorf("record: internal note requires a non-empty string")
	}
	return nil
}

// Reason is the structured "why" carried by every audit record: a ReasonKind
// tagged with the VisibilityTier its kind is allowed to hold, plus the payload
// for exactly that tier.
//
// Every field is unexported and there are no setters, so a Reason can only be
// built through the three tier-specific constructors, each of which admits only
// the evidence type for its tier. There is deliberately no builder that takes a
// tier: a single builder would be one code path able to emit any tier, which is
// exactly the path the type design exists to remove.
type Reason struct {
	kind  ReasonKind
	tier  VisibilityTier
	obs   *ObservedEvidence
	rec   *ReconstructedEvidence
	inter *InternalNote
}

// NewObservedReason builds a Reason for an Observed tier. It fails unless kind's
// allowed tier is Observed, and unless ev is a valid observed evidence value.
func NewObservedReason(kind ReasonKind, ev ObservedEvidence) (Reason, error) {
	want, ok := kind.AllowedTier()
	if !ok {
		return Reason{}, fmt.Errorf("record: unknown reason kind %q", kind)
	}
	if want != Observed {
		return Reason{}, fmt.Errorf("record: reason %s requires tier %s, got %s", kind, want, Observed)
	}
	if err := ev.validate(); err != nil {
		return Reason{}, fmt.Errorf("record: observed reason %s: %w", kind, err)
	}
	c := ev.clone()
	return Reason{kind: kind, tier: Observed, obs: &c}, nil
}

// NewReconstructedReason builds a Reason for a Reconstructed tier. It fails
// unless kind's allowed tier is Reconstructed, and unless ev is a valid
// reconstructed evidence value.
func NewReconstructedReason(kind ReasonKind, ev ReconstructedEvidence) (Reason, error) {
	want, ok := kind.AllowedTier()
	if !ok {
		return Reason{}, fmt.Errorf("record: unknown reason kind %q", kind)
	}
	if want != Reconstructed {
		return Reason{}, fmt.Errorf("record: reason %s requires tier %s, got %s", kind, want, Reconstructed)
	}
	if err := ev.validate(); err != nil {
		return Reason{}, fmt.Errorf("record: reconstructed reason %s: %w", kind, err)
	}
	c := ev.clone()
	return Reason{kind: kind, tier: Reconstructed, rec: &c}, nil
}

// NewInternalReason builds a Reason for an Internal tier. It fails unless
// kind's allowed tier is Internal, and unless note is a valid internal note.
func NewInternalReason(kind ReasonKind, note InternalNote) (Reason, error) {
	want, ok := kind.AllowedTier()
	if !ok {
		return Reason{}, fmt.Errorf("record: unknown reason kind %q", kind)
	}
	if want != Internal {
		return Reason{}, fmt.Errorf("record: reason %s requires tier %s, got %s", kind, want, Internal)
	}
	if err := note.validate(); err != nil {
		return Reason{}, fmt.Errorf("record: internal reason %s: %w", kind, err)
	}
	c := note
	return Reason{kind: kind, tier: Internal, inter: &c}, nil
}

// Kind returns the reason's kind.
func (r Reason) Kind() ReasonKind { return r.kind }

// Tier returns the reason's visibility tier.
func (r Reason) Tier() VisibilityTier { return r.tier }

// Observed returns a copy of the observed evidence and true when the reason's
// tier is Observed; otherwise it returns the zero value and false.
func (r Reason) Observed() (ObservedEvidence, bool) {
	if r.tier != Observed || r.obs == nil {
		return ObservedEvidence{}, false
	}
	return r.obs.clone(), true
}

// Reconstructed returns a copy of the reconstructed evidence and true when the
// reason's tier is Reconstructed; otherwise it returns the zero value and false.
func (r Reason) Reconstructed() (ReconstructedEvidence, bool) {
	if r.tier != Reconstructed || r.rec == nil {
		return ReconstructedEvidence{}, false
	}
	return r.rec.clone(), true
}

// InternalNote returns a copy of the internal note and true when the reason's
// tier is Internal; otherwise it returns the zero value and false.
func (r Reason) InternalNote() (InternalNote, bool) {
	if r.tier != Internal || r.inter == nil {
		return InternalNote{}, false
	}
	return *r.inter, true
}

// Validate reports whether the reason is well formed: its tier is one of the
// three real tiers, its kind is known, its kind's allowed tier is its own tier,
// and the evidence payload for that tier is present. It returns a non-nil error
// otherwise, including for the zero value of Reason.
func (r Reason) Validate() error {
	if !r.tier.Valid() {
		return fmt.Errorf("record: reason has an invalid visibility tier")
	}
	want, ok := r.kind.AllowedTier()
	if !ok {
		return fmt.Errorf("record: reason has unknown kind %q", r.kind)
	}
	if want != r.tier {
		return fmt.Errorf("record: reason kind %s requires tier %s, got %s", r.kind, want, r.tier)
	}
	switch r.tier {
	case Observed:
		if r.obs == nil {
			return fmt.Errorf("record: observed reason %s is missing its observed evidence", r.kind)
		}
	case Reconstructed:
		if r.rec == nil {
			return fmt.Errorf("record: reconstructed reason %s is missing its reconstructed evidence", r.kind)
		}
	case Internal:
		if r.inter == nil {
			return fmt.Errorf("record: internal reason %s is missing its internal note", r.kind)
		}
	}
	return nil
}

// reasonEncodingVersion tags every Encode output so a future format change can
// be detected rather than silently mis-parsed.
const reasonEncodingVersion = "notary/reason/v1"

// reasonEnvelope is the wire form emitted by Encode and read by ParseReason. It
// is deliberately a plain struct with exported-for-encoding fields; it never
// escapes this file.
type reasonEnvelope struct {
	Version       string            `json:"v"`
	Kind          ReasonKind        `json:"kind"`
	Tier          VisibilityTier    `json:"tier"`
	Observed      *observedEnvelope `json:"observed,omitempty"`
	Reconstructed *reconstrEnvelope `json:"reconstructed,omitempty"`
	Internal      *internalEnvelope `json:"internal,omitempty"`
}

// observedEnvelope is the wire form of an ObservedEvidence.
type observedEnvelope struct {
	Source  string          `json:"source"`
	Payload json.RawMessage `json:"payload"`
}

// reconstrEnvelope is the wire form of a ReconstructedEvidence.
type reconstrEnvelope struct {
	Basis       []RecordID `json:"basis"`
	Rule        string     `json:"rule"`
	RuleVersion string     `json:"rule_version"`
	Confidence  float64    `json:"confidence"`
}

// internalEnvelope is the wire form of an InternalNote.
type internalEnvelope struct {
	Opaque string `json:"opaque"`
}

// Encode serialises the reason to deterministic, self-describing bytes: the
// version tag, the kind, the tier, and the payload for that tier. It is an
// error to encode an invalid reason.
//
// Encode exists because internal/store lives in a different package and cannot
// read Reason's unexported fields; the store persists these opaque bytes and
// ParseReason reconstructs the value.
func (r Reason) Encode() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("record: encode reason: %w", err)
	}

	env := reasonEnvelope{
		Version: reasonEncodingVersion,
		Kind:    r.kind,
		Tier:    r.tier,
	}
	switch r.tier {
	case Observed:
		env.Observed = &observedEnvelope{
			Source:  evidenceSourceName(r.obs.source),
			Payload: json.RawMessage(r.obs.payload),
		}
	case Reconstructed:
		env.Reconstructed = &reconstrEnvelope{
			Basis:       r.rec.basis,
			Rule:        r.rec.rule,
			RuleVersion: r.rec.ruleVersion,
			Confidence:  r.rec.confidence,
		}
	case Internal:
		env.Internal = &internalEnvelope{Opaque: r.inter.opaque}
	}

	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("record: encode reason: %w", err)
	}
	return b, nil
}

// ParseReason reconstructs a Reason from Encode output. Every value is rebuilt
// through the tier-specific constructors, so a row tampered with in storage
// fails validation rather than silently becoming a different claim. Unknown
// version tags, malformed input, and tier/kind mismatches all return an error;
// it never panics and never yields a partially valid reason.
func ParseReason(data []byte) (Reason, error) {
	var env reasonEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Reason{}, fmt.Errorf("record: parse reason: %w", err)
	}
	if env.Version != reasonEncodingVersion {
		return Reason{}, fmt.Errorf("record: unknown reason encoding version %q", env.Version)
	}
	if !env.Tier.Valid() {
		return Reason{}, fmt.Errorf("record: reason carries an invalid visibility tier")
	}

	switch {
	case env.Tier == Observed:
		if env.Observed == nil {
			return Reason{}, fmt.Errorf("record: observed reason %s is missing its observed payload", env.Kind)
		}
		src, err := parseEvidenceSource(env.Observed.Source)
		if err != nil {
			return Reason{}, err
		}
		ev, err := NewObservedEvidence(src, []byte(env.Observed.Payload))
		if err != nil {
			return Reason{}, err
		}
		return NewObservedReason(env.Kind, ev)
	case env.Tier == Reconstructed:
		if env.Reconstructed == nil {
			return Reason{}, fmt.Errorf("record: reconstructed reason %s is missing its reconstructed payload", env.Kind)
		}
		ev, err := NewReconstructedEvidence(
			env.Reconstructed.Basis,
			env.Reconstructed.Rule,
			env.Reconstructed.RuleVersion,
			env.Reconstructed.Confidence,
		)
		if err != nil {
			return Reason{}, err
		}
		return NewReconstructedReason(env.Kind, ev)
	case env.Tier == Internal:
		if env.Internal == nil {
			return Reason{}, fmt.Errorf("record: internal reason %s is missing its internal payload", env.Kind)
		}
		note, err := NewInternalNote(env.Internal.Opaque)
		if err != nil {
			return Reason{}, err
		}
		return NewInternalReason(env.Kind, note)
	default:
		return Reason{}, fmt.Errorf("record: reason carries an invalid visibility tier")
	}
}

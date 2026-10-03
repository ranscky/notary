// Package export renders the ledger as JSONL: one stable JSON object per
// record, streamed in range order. It is a read path over internal/ledger and
// writes nothing of its own.
package export

import (
	"encoding/hex"
	"fmt"
	"time"

	"notary/internal/record"
)

// Line is the JSONL shape of one exported record: the fixed set of structured
// fields an auditor reads, one object per line, in a stable order.
//
// It is a plain struct with json tags rather than a map, so the field order is
// a property of the type and cannot drift between renders. Hash-like fields are
// lowercase hex strings, matching the checkpoint wire form, rather than JSON
// arrays of bytes.
//
// Later tasks extend this shape: Task 5 added the deterministic `phrasing`
// sentence, Task 6 adds redaction (setting `redacted` and dropping `content`),
// and Task 10 adds the display-only `paraphrase` object.
type Line struct {
	Seq         uint64                `json:"seq"`
	ID          record.RecordID       `json:"id"`
	At          time.Time             `json:"at"`
	RecordedAt  time.Time             `json:"recorded_at"`
	Event       record.EventType      `json:"event"`
	Tier        record.VisibilityTier `json:"tier"`
	ReasonKind  record.ReasonKind     `json:"reason_kind"`
	MemoryID    string                `json:"memory_id"`
	Scope       LineScope             `json:"scope"`
	ContentHash string                `json:"content_hash"`
	// Content is the memory text, present only when the record carried content.
	// It is a pointer so "no content" (omit) and "empty content" (a non-nil
	// pointer to "") are distinguishable.
	Content *string `json:"content,omitempty"`
	// Redacted names why content was withheld. It is absent when content was
	// shown; redaction is Task 6.
	Redacted string `json:"redacted,omitempty"`
	// Phrasing restates the record's claim as one human sentence (Task 5). It
	// is derived mechanically from Event, tier, reason kind and memory id, so it
	// adds no claim of its own. It is always present and always printed BESIDE
	// the structured fields above it, never instead of them: a reader may
	// ignore it, and no consumer may depend on it (design §2, §7).
	Phrasing    string `json:"phrasing"`
	PrevHash    string `json:"prev_hash"`
	Hash        string `json:"hash"`
	Signature   string `json:"signature"`
	SignerKeyID string `json:"signer_key_id"`
}

// LineScope is the caller context a record belongs to, as it appears in the
// JSONL. Empty dimensions are omitted, so a line shows only the scope the
// record actually carried.
type LineScope struct {
	UserID  string `json:"user_id,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	AppID   string `json:"app_id,omitempty"`
	RunID   string `json:"run_id,omitempty"`
}

// Render turns one record into its JSONL shape.
//
// includeSensitive is accepted so the signature is stable for the redaction
// task that will consult it; no redaction exists yet, so content is rendered
// whenever the record carries any and `redacted` is never set.
//
// Content is rendered only when the record carries a non-nil Content: a record
// with no content (a derived record, or an audit_gap) has no `content` field at
// all, so a consumer can tell "nothing was recorded" from "you are not cleared
// for it". A non-nil Content with empty Text renders an empty string, which is
// content that exists and is empty -- distinct from no content at all.
//
// Render returns an error only for a record it cannot render honestly: one
// whose reason carries no valid tier, or one whose claim has no phrasing. The
// latter is unreachable for a record the production paths build -- phrasing is
// total over those (see Phrase and TestPhraseIsTotalOverConstructibleRecords)
// -- so it fails loudly rather than emitting a line whose prose reads as a
// missing record.
func Render(rec record.Record, includeSensitive bool) (Line, error) {
	_ = includeSensitive // Redaction is Task 6; nothing consults the flag yet.

	tier := rec.Reason.Tier()
	if !tier.Valid() {
		return Line{}, fmt.Errorf("export: record %s has an invalid visibility tier", rec.ID)
	}

	phrasing, ok := Phrase(rec)
	if !ok {
		return Line{}, fmt.Errorf("export: record %s has no phrasing for event %s with reason %s",
			rec.ID, rec.Event, rec.Reason.Kind())
	}

	line := Line{
		Seq:        rec.Seq,
		ID:         rec.ID,
		At:         rec.At,
		RecordedAt: rec.RecordedAt,
		Event:      rec.Event,
		Tier:       tier,
		ReasonKind: rec.Reason.Kind(),
		MemoryID:   rec.Subject.MemoryID,
		Scope: LineScope{
			UserID:  rec.Subject.Scope.UserID,
			AgentID: rec.Subject.Scope.AgentID,
			AppID:   rec.Subject.Scope.AppID,
			RunID:   rec.Subject.Scope.RunID,
		},
		ContentHash: hex.EncodeToString(rec.Subject.ContentHash[:]),
		Phrasing:    phrasing,
		PrevHash:    hex.EncodeToString(rec.PrevHash[:]),
		Hash:        hex.EncodeToString(rec.Hash[:]),
		Signature:   hex.EncodeToString(rec.Signature),
		SignerKeyID: rec.SignerKeyID,
	}
	if rec.Content != nil {
		text := rec.Content.Text
		line.Content = &text
	}
	return line, nil
}

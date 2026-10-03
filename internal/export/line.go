// Package export renders the ledger as JSONL: one stable JSON object per
// record, streamed in range order. It is a read path over internal/ledger; it
// never writes the ledger, and its only output of its own is the optional
// signed head checkpoint a request may ask for via Request.CheckpointOut.
package export

import (
	"encoding/hex"
	"fmt"
	"time"

	"notary/internal/phrase"
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
// sentence, Task 6 added redaction (setting `redacted` and dropping `content`
// for sensitive records rendered without IncludeSensitive), and Task 10 adds
// the display-only `paraphrase` object.
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
	// Redacted names why content was withheld: it is set to redactedSensitive
	// when the stored record marked its content sensitive and the export was
	// not asked to include sensitive content. It is absent whenever content was
	// shown -- and, critically, whenever the record carried no content at all,
	// so a consumer can always tell "nothing was recorded" from "you are not
	// cleared for it".
	Redacted string `json:"redacted,omitempty"`
	// Phrasing restates the record's claim as one human sentence (Task 5). It
	// is derived mechanically from Event, tier, reason kind and memory id, so it
	// adds no claim of its own. It is always present and always printed BESIDE
	// the structured fields above it, never instead of them: a reader may
	// ignore it, and no consumer may depend on it (design §2, §7).
	Phrasing string `json:"phrasing"`
	// Paraphrase is the optional language model's restatement of the record's
	// claim, present only when the export ran with Phrase set and the provider
	// returned one. It sits BESIDE the structured fields, never instead of them,
	// and is labelled a paraphrase. It is display-only: no decision package may
	// IMPORT internal/phrase (design D7), so none can name the type -- but this
	// field is exactly how generated text is reachable BY VALUE without that
	// import, so the isolation is of the type, not of the data. It is a pointer so its
	// absence -- no --phrase, or a failed call -- is distinguishable from a
	// paraphrase, and because the client never returns an empty one there is no
	// empty object to mistake for a real paraphrase.
	Paraphrase  *phrase.Paraphrase `json:"paraphrase,omitempty"`
	PrevHash    string             `json:"prev_hash"`
	Hash        string             `json:"hash"`
	Signature   string             `json:"signature"`
	SignerKeyID string             `json:"signer_key_id"`
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

// redactedSensitive is the value of Line.Redacted when content was withheld
// because the stored record marked it sensitive.
//
// It names the FLAG -- the evidentiary fact -- not the rule that produced it.
// record.Content is {Text, Sensitive} and both are mixed into the record hash,
// so persisting which rule fired would change the digest of every record ever
// written; the flag is what is stored, and which rule set it is declarative
// configuration, reproducible from the rules file (design §6).
const redactedSensitive = "sensitive"

// Render turns one record into its JSONL shape.
//
// includeSensitive decides whether a record whose stored content is marked
// sensitive has its text shown. When it is false (the safe default) the text is
// WITHHELD and Redacted is set to redactedSensitive, so the withheld line still
// states why it is empty. When it is true the text is shown and Redacted is
// left absent. Redaction removes only the text: it never touches the hash, the
// chain fields, or the fact that content existed, because rendering is a read
// path and the hash was fixed at Append.
//
// Content is rendered only when the record carries a non-nil Content: a record
// with no content (a derived record, or an audit_gap) has no `content` field at
// all AND no `redacted` claim, so a consumer can tell "nothing was recorded"
// from "you are not cleared for it". A non-nil Content with empty Text is
// content that exists and is empty -- distinct from no content at all -- and if
// it is sensitive it is redacted like any other sensitive content.
//
// Render returns an error only for a record it cannot render honestly: one
// whose reason carries no valid tier, or one whose claim has no phrasing. The
// latter is unreachable for a record the production paths build -- phrasing is
// total over those (see Phrase and TestPhraseIsTotalOverTheVocabulary)
// -- so it fails loudly rather than emitting a line whose prose reads as a
// missing record.
func Render(rec record.Record, includeSensitive bool) (Line, error) {
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
		if rec.Content.Sensitive && !includeSensitive {
			// Withhold the text and say so. Nothing else on the line -- the
			// content hash, the chain fields, the record's identity -- is
			// touched: redaction is a rendering decision, and the evidence was
			// fixed at Append.
			line.Redacted = redactedSensitive
		} else {
			text := rec.Content.Text
			line.Content = &text
		}
	}
	return line, nil
}

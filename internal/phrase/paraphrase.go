package phrase

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"notary/internal/record"
)

// Paraphrase is a language model's restatement of a run of records' claims.
//
// It is display-only. The design (D7) makes that a property of the import graph
// rather than of review: the type lives in this package, and no decision package
// IMPORTS this package, so none can even NAME the type. That is the direct-import
// guarantee only: a package may still read this value through
// export.Line.Paraphrase without importing phrase, so "generated text is never an
// input to a decision" rests on the render path feeding no decision, not on the
// import graph alone. It is rendered beside the structured record and its tier,
// never instead of them, and labelled a paraphrase.
//
// Text is the model's sentence, already trimmed. Model names what produced it --
// the model the provider reported, or the configured model when the response
// did not echo one. At is when the call returned.
//
// The json tags make the object's wire keys snake_case, matching every sibling
// key in an exported line (content_hash, reason_kind, signer_key_id). At is a
// time.Time, so it encodes as an RFC3339 string exactly as the line's own `at`
// and `recorded_at` do -- one date format across the whole object.
type Paraphrase struct {
	Text  string    `json:"text"`
	Model string    `json:"model"`
	At    time.Time `json:"at"`
}

// roleSystem and roleUser are the two chat roles a request uses.
const (
	roleSystem = "system"
	roleUser   = "user"
)

// systemPrompt instructs the model to restate the structured claim and
// explicitly withholds the memory's text, so the instruction and the payload
// agree rather than the model being invited to guess content it was not given.
const systemPrompt = "You restate an audit claim about an AI agent's memory in one short, plain-English " +
	"sentence. Each claim is given only as structured metadata. You are never given the memory's " +
	"text, so never invent or guess it. Reply with the sentence alone and no preamble."

// message is one entry of the OpenAI-compatible messages array.
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the compatible chat-completions request body. It carries ONLY
// model and messages (D2): a richer request would appear to work against a
// provider whose compatible layer silently ignores unknown fields, while
// quietly dropping the extra intent. Any field added here weakens that
// guarantee, and TestRequestBodyCarriesOnlyModelAndMessages is what catches it.
type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
}

// chatResponse is the slice of the compatible response shape the client reads.
// Every other field a provider sends -- id, object, created, usage, an unknown
// vendor extension -- is simply not named here, so it is ignored rather than
// refused.
type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// buildMessages renders records as a compatible messages array.
//
// It reads ONLY structured fields -- event, tier, reason kind, scope, memory id
// and content hash. A record's Content.Text is deliberately never touched, so
// the request cannot carry text that redaction exists to protect (design §8):
// a paraphrase cannot leak what it was never given.
func buildMessages(records []record.Record) []message {
	var b strings.Builder
	b.WriteString("Restate each claim below in one sentence, using only the metadata given:\n")
	for i, rec := range records {
		fmt.Fprintf(&b, "\nclaim %d:\n", i+1)
		fmt.Fprintf(&b, "  event: %s\n", rec.Event)
		fmt.Fprintf(&b, "  tier: %s\n", rec.Reason.Tier())
		fmt.Fprintf(&b, "  reason: %s\n", rec.Reason.Kind())
		writeScope(&b, rec.Subject.Scope)
		if rec.Subject.MemoryID != "" {
			fmt.Fprintf(&b, "  memory_id: %s\n", rec.Subject.MemoryID)
		}
		fmt.Fprintf(&b, "  content_hash: %s\n", hex.EncodeToString(rec.Subject.ContentHash[:]))
	}
	return []message{
		{Role: roleSystem, Content: systemPrompt},
		{Role: roleUser, Content: b.String()},
	}
}

// writeScope writes the non-empty dimensions of a scope as one line. An empty
// scope dimension is omitted so the prompt shows only the context the record
// actually carried.
func writeScope(b *strings.Builder, s record.Scope) {
	parts := make([]string, 0, 4)
	if s.UserID != "" {
		parts = append(parts, "user_id="+s.UserID)
	}
	if s.AgentID != "" {
		parts = append(parts, "agent_id="+s.AgentID)
	}
	if s.AppID != "" {
		parts = append(parts, "app_id="+s.AppID)
	}
	if s.RunID != "" {
		parts = append(parts, "run_id="+s.RunID)
	}
	if len(parts) == 0 {
		return
	}
	fmt.Fprintf(b, "  scope: %s\n", strings.Join(parts, " "))
}

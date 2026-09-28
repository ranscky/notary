// Package record defines the core, unforgeable value types that make up
// Notary's audit trail: the visibility tier that labels how Notary came to
// know a memory, and the shared identifiers the rest of the package builds on.
package record

import (
	"encoding/json"
	"fmt"
)

// tierValue is the unexported discriminant that backs a VisibilityTier.
//
// It is deliberately unexported: because the sole field of VisibilityTier is
// unexported, no code outside this package can construct a tier by enum value
// and no caller can forge one. External code must use the Observed,
// Reconstructed, and Internal variables (or UnmarshalJSON).
type tierValue uint8

const (
	// tierInvalid is the zero value of tierValue. A VisibilityTier holding it
	// is never valid, so the zero value of VisibilityTier can never be
	// mistaken for a real tier.
	tierInvalid tierValue = iota
	tierObserved
	tierReconstructed
	tierInternal
)

// VisibilityTier labels how Notary came to know something:
//
//   - Observed: captured directly from an interaction or event.
//   - Reconstructed: inferred or rebuilt from other records.
//   - Internal: produced by Notary itself, not sourced from a model or user.
//
// A VisibilityTier must be one of the exported variables; the unexported
// field makes forging impossible. The zero value is invalid and Valid reports
// false for it, so an unintentionally defaulted tier is always detectable.
type VisibilityTier struct {
	v tierValue
}

var (
	// Observed marks information captured directly from an interaction or event.
	Observed = VisibilityTier{tierObserved}
	// Reconstructed marks information inferred or rebuilt from other records.
	Reconstructed = VisibilityTier{tierReconstructed}
	// Internal marks information produced by Notary itself.
	Internal = VisibilityTier{tierInternal}
)

// Valid reports whether t is one of the three well-defined tiers. It returns
// false for the zero value of VisibilityTier.
func (t VisibilityTier) Valid() bool {
	switch t.v {
	case tierObserved, tierReconstructed, tierInternal:
		return true
	default:
		return false
	}
}

// String returns the lowercase wire name of the tier: "observed",
// "reconstructed", or "internal". For an invalid tier it returns "invalid".
func (t VisibilityTier) String() string {
	switch t.v {
	case tierObserved:
		return "observed"
	case tierReconstructed:
		return "reconstructed"
	case tierInternal:
		return "internal"
	default:
		return "invalid"
	}
}

// MarshalJSON encodes the tier as its quoted String() form, for example
// "observed". Marshalling an invalid tier (including the zero value) is an
// error rather than a silently defaulted label, so a forged or defaulted tier
// can never reach the audit trail.
func (t VisibilityTier) MarshalJSON() ([]byte, error) {
	if !t.Valid() {
		return nil, fmt.Errorf("record: cannot marshal invalid visibility tier")
	}
	return json.Marshal(t.String())
}

// UnmarshalJSON decodes one of the three tier strings ("observed",
// "reconstructed", or "internal"). Any other input, including a non-string
// JSON value, returns a non-nil error and leaves the receiver invalid, so a
// failed decode can never be mistaken for a valid tier.
func (t *VisibilityTier) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		*t = VisibilityTier{}
		return fmt.Errorf("record: visibility tier must be a JSON string: %w", err)
	}

	switch s {
	case "observed":
		*t = Observed
	case "reconstructed":
		*t = Reconstructed
	case "internal":
		*t = Internal
	default:
		*t = VisibilityTier{}
		return fmt.Errorf("record: unknown visibility tier %q", s)
	}
	return nil
}

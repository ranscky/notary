package interceptor

import "notary/internal/record"

// Rule is one declarative sensitivity rule: a named matcher that, when it
// matches a record about to be written, marks that record's content sensitive.
// Rules are configuration, never code -- an operator states "anything in this
// scope, or carrying this Mem0 metadata key, is sensitive" in a file rather than
// in a call site.
//
// Rule is deliberately distinct from reconcile.Rule. That type is the claim
// registry: the stable, signed names of the inferences the reconciler may make.
// This type decides redaction. They share an English word and nothing else, so
// neither is named after the other.
type Rule struct {
	// Name identifies the rule. It is reported when the rule matches (as the
	// "rule:<name>" redaction reason) and named in load errors, so an operator
	// can tell which rule did what. It is not required to be unique.
	Name string
	// Scope is the scope clause. A non-empty field must equal the record's
	// corresponding scope field; an empty field is "any". The zero Scope is no
	// scope clause at all, which matches every scope.
	Scope record.Scope
	// MetadataKey is the Mem0 metadata key clause. An empty key means no
	// metadata clause, and the rule matches any metadata.
	MetadataKey string
	// MetadataValue is the value MetadataKey must have for the metadata clause
	// to match. It is a string because Mem0 metadata values are matched as
	// strings; a metadata value of any other type never matches (see Match).
	MetadataValue string
}

// RuleSet holds sensitivity rules in evaluation order. Order is the slice's, not
// a map's, so first-match-wins is deterministic and never depends on map
// iteration.
type RuleSet struct {
	rules []Rule
}

// NewRuleSet returns a RuleSet over rules in the given order. A nil slice is
// valid and matches nothing. The rules are not copied: the slice is read-only
// by contract, and a caller that mutates it after construction does so at its
// own risk.
func NewRuleSet(rules []Rule) *RuleSet {
	return &RuleSet{rules: rules}
}

// Match reports the name of the first rule that matches scope and metadata, and
// whether any matched. It returns ("", false) when no rule matches, and a
// nil receiver is treated as an empty rule set rather than panicking.
//
// A rule matches when every clause it specifies matches:
//
//   - A rule with only a scope clause matches any metadata.
//   - A rule with only a metadata clause matches any scope.
//   - A rule with both matches only when both match.
//   - A rule with neither specifies nothing and so matches every record
//     (vacuous truth). Such a rule is rejected by config.LoadSensitivityRules
//     at load time, so it is never reachable from a configuration file; the
//     semantics here remain the literal ones.
//
// Metadata is Mem0's map[string]any. A metadata clause matches only when the
// key is present AND its value is a string equal to MetadataValue: a value of
// any other type (a number, a bool, a list, a nested map, nil) is not a string
// and so does not match a string rule. A nil metadata map matches no metadata
// clause.
func (rs *RuleSet) Match(scope record.Scope, metadata map[string]any) (name string, matched bool) {
	if rs == nil {
		return "", false
	}
	for _, r := range rs.rules {
		if r.matches(scope, metadata) {
			return r.Name, true
		}
	}
	return "", false
}

// matches reports whether r matches scope and metadata, per Match's documented
// semantics.
func (r Rule) matches(scope record.Scope, metadata map[string]any) bool {
	return r.matchesScope(scope) && r.matchesMetadata(metadata)
}

// matchesScope reports whether the rule's scope clause matches scope. Every
// non-empty rule field must equal scope's field; an empty field matches
// anything. The zero Scope therefore matches every scope.
func (r Rule) matchesScope(scope record.Scope) bool {
	if r.Scope.UserID != "" && r.Scope.UserID != scope.UserID {
		return false
	}
	if r.Scope.AgentID != "" && r.Scope.AgentID != scope.AgentID {
		return false
	}
	if r.Scope.AppID != "" && r.Scope.AppID != scope.AppID {
		return false
	}
	if r.Scope.RunID != "" && r.Scope.RunID != scope.RunID {
		return false
	}
	return true
}

// matchesMetadata reports whether the rule's metadata clause matches metadata.
// An empty MetadataKey is no clause and matches anything. Otherwise the key must
// be present and hold a string equal to MetadataValue; any non-string value does
// not match.
func (r Rule) matchesMetadata(metadata map[string]any) bool {
	if r.MetadataKey == "" {
		return true
	}
	v, ok := metadata[r.MetadataKey]
	if !ok {
		return false
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	return s == r.MetadataValue
}

package library

import "notary/internal/interceptor"

// Option configures a Mem0Interceptor at construction. It is deliberately a
// different family from CallOption. An Option is applied once, by New, and
// shapes how every record the interceptor writes is classified; a CallOption is
// applied to a single Add or Search and shapes only that call. The two are
// separate types with separate names so a construction-time decision can never
// be confused with a per-call one.
type Option func(*Mem0Interceptor)

// WithSensitivityRules configures the interceptor to consult rs for every
// record it is about to write. Where a rule matches the record's scope and the
// Mem0 metadata available at that point, the record's content is marked
// sensitive -- set on record.Content before the record is hashed -- exactly as
// if the caller had passed Sensitive().
//
// The two input paths are an OR: either the caller's option or a matching rule
// marks the content, and there is deliberately no way to mark content
// non-sensitive against a matching rule, because the safe direction is the only
// one worth having.
//
// A nil rs is valid and matches nothing, so it is the same as omitting the
// option and cannot make the interceptor panic.
func WithSensitivityRules(rs *interceptor.RuleSet) Option {
	return func(m *Mem0Interceptor) { m.rules = rs }
}

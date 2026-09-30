// Package reconcile derives the claims Notary can only infer: what became of an
// add after it was acknowledged, whether a known memory still exists, and
// whether Mem0 removed one without saying why.
//
// It is the sole producer of Reconstructed and Internal records. Those tiers
// rest on an inference, and every inference is named by the rule that produced
// it. This file is that name registry.
//
// A rule name and version travel inside the signed record: a
// record.ReconstructedEvidence carries both, and NewReconstructedEvidence
// refuses an empty rule or rule version. So a name is a permanent, signed
// contract. Renaming a rule would leave every earlier record unexplained, and a
// version bumps only when the rule's meaning changes — never for a refactor, a
// rename, or a reworded summary. Declaring every name here, once, is what stops
// a call site from inventing its own string literal and drifting from that
// contract.
package reconcile

import "notary/internal/record"

// Rule names one inference the reconciler is allowed to make: the stable name
// recorded in the signed record, the version of its meaning, the reason kind it
// justifies, and a plain-language summary for a reader years later.
type Rule struct {
	// Name is the rule's permanent identifier, e.g. "absent_from_search". It is
	// written into signed records, so it is never renamed or reused for a
	// different meaning.
	Name string
	// Version is the rule's meaning version. It is written into signed records
	// alongside Name. It is bumped only when what the rule asserts changes.
	Version string
	// Kind is the reason kind a claim justified by this rule carries.
	Kind record.ReasonKind
	// Summary states what the rule asserts, in the words of the design spec, so
	// an auditor can establish what "rule <Name> v<Version>" was claiming.
	Summary string
}

// The v1 rule names. They are constants so producers record the identical
// string, and so a typo is a compile error rather than a new signed rule name.
const (
	// RuleKeptByContentMatch justifies kept_by_content_match.
	RuleKeptByContentMatch = "kept_by_content_match"
	// RuleAbsentFromSearch justifies absent_from_search.
	RuleAbsentFromSearch = "absent_from_search"
	// RuleNoFactsExtracted justifies no_facts_extracted.
	RuleNoFactsExtracted = "no_facts_extracted"
	// RuleRemovedByMem0 justifies removed_by_mem0.
	RuleRemovedByMem0 = "removed_by_mem0"
)

// ruleVersionV1 is the version of every rule whose meaning is the one described
// in this file's v1 table.
const ruleVersionV1 = "1"

// v1Rules is the complete v1 rule set.
//
// It is a function rather than a package-level variable on purpose: there is no
// global mutable state, so no code path can reassign the registry at run time,
// and every caller owns the slice it receives and may freely mutate it without
// disturbing any other caller.
func v1Rules() []Rule {
	return []Rule{
		{
			Name:    RuleKeptByContentMatch,
			Version: ruleVersionV1,
			Kind:    record.ReasonKeptByContentMatch,
			Summary: "memory present in a complete enumeration whose id differs from the add's produced id, " +
				"but whose content hash equals the submitted text",
		},
		{
			Name:    RuleAbsentFromSearch,
			Version: ruleVersionV1,
			Kind:    record.ReasonAbsentFromSearch,
			Summary: "known memory absent from a search that was saturated (len(results) < top_k) and did not return that memory",
		},
		{
			Name:    RuleNoFactsExtracted,
			Version: ruleVersionV1,
			Kind:    record.ReasonNoFactsExtracted,
			Summary: "event resolved SUCCEEDED with an empty results array",
		},
		{
			Name:    RuleRemovedByMem0,
			Version: ruleVersionV1,
			Kind:    record.ReasonRemovedByMem0,
			Summary: "absent from a complete enumeration, and history shows DELETE/UPDATE exposing no reason",
		},
	}
}

// Rules returns every registered rule. The result is a fresh slice the caller
// owns, so a caller cannot mutate the registry through it.
func Rules() []Rule {
	return v1Rules()
}

// LookupRule returns the rule registered under name, and whether it exists. An
// unknown name reports false rather than a zero rule, so a caller cannot mistake
// a missing rule for a real one.
func LookupRule(name string) (Rule, bool) {
	for _, r := range v1Rules() {
		if r.Name == name {
			return r, true
		}
	}
	return Rule{}, false
}

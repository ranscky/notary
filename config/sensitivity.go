package config

import (
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"

	"notary/internal/interceptor"
	"notary/internal/record"
)

// LoadSensitivityRules reads the declarative sensitivity-rules YAML file at path
// and returns the rules it holds, in file order. The path comes from
// EnvSensitivityRules (NOTARY_SENSITIVITY_RULES); Load does not read it, because
// the rules belong to whatever application constructs the interceptor, at the
// point the interceptor is constructed -- no notary command writes records, so
// the rules are not a CLI concern.
//
// The document shape is:
//
//	rules:
//	  - name: health-data
//	    match:
//	      scope:
//	        user_id: patient-42      # optional; omitted means any scope
//	      metadata:
//	        category: health         # optional; matches Mem0 metadata key=value
//
// A rule matches when every clause it specifies matches (see
// interceptor.RuleSet.Match). A rule specifying neither a scope clause nor a
// metadata clause is rejected here, with an error naming the rule: it would
// vacuously match every record and mark an entire ledger sensitive by accident,
// and that must be loud at startup rather than discovered in an export.
func LoadSensitivityRules(path string) ([]interceptor.Rule, error) {
	if path == "" {
		return nil, fmt.Errorf(
			"config: %s names no rules file path", EnvSensitivityRules)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read sensitivity rules %s: %w", path, err)
	}

	var doc sensitivityDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("config: parse sensitivity rules %s: %w", path, err)
	}

	rules := make([]interceptor.Rule, 0, len(doc.Rules))
	for i, yr := range doc.Rules {
		rule, err := yr.toRule(labelFor(i, yr.Name))
		if err != nil {
			return nil, fmt.Errorf("config: sensitivity rules %s: %w", path, err)
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// sensitivityDocument is the file's top-level shape.
type sensitivityDocument struct {
	Rules []sensitivityRule `yaml:"rules"`
}

// sensitivityRule is one entry under rules:, as written on disk.
type sensitivityRule struct {
	Name  string                 `yaml:"name"`
	Match sensitivityRuleClauses `yaml:"match"`
}

// sensitivityRuleClauses is a rule's match: block. Every clause is optional.
type sensitivityRuleClauses struct {
	Scope    sensitivityScope  `yaml:"scope"`
	Metadata map[string]string `yaml:"metadata"`
}

// sensitivityScope is a scope clause. An omitted or empty field means "any".
type sensitivityScope struct {
	UserID  string `yaml:"user_id"`
	AgentID string `yaml:"agent_id"`
	AppID   string `yaml:"app_id"`
	RunID   string `yaml:"run_id"`
}

// toRule converts a parsed rule to an interceptor.Rule, rejecting the shapes the
// Rule type cannot faithfully represent. label names the rule in error messages.
func (yr sensitivityRule) toRule(label string) (interceptor.Rule, error) {
	scope := record.Scope{
		UserID:  yr.Match.Scope.UserID,
		AgentID: yr.Match.Scope.AgentID,
		AppID:   yr.Match.Scope.AppID,
		RunID:   yr.Match.Scope.RunID,
	}

	var (
		key   string
		value string
	)
	switch len(yr.Match.Metadata) {
	case 0:
		// No metadata clause.
	case 1:
		for k, v := range yr.Match.Metadata {
			key, value = k, v
		}
	default:
		// The Rule type carries exactly one metadata key/value pair, so a
		// multi-key clause cannot be represented. Silently keeping whichever
		// key a map iteration yields first would make the rule's effect depend
		// on map order, so this is rejected instead.
		return interceptor.Rule{}, fmt.Errorf(
			"rule %s: metadata names %d keys; a rule matches exactly one metadata key/value pair",
			label, len(yr.Match.Metadata))
	}

	if scope == (record.Scope{}) && key == "" {
		return interceptor.Rule{}, fmt.Errorf(
			"rule %s: specifies neither a scope nor a metadata clause, which would mark every record sensitive",
			label)
	}

	return interceptor.Rule{
		Name:          yr.Name,
		Scope:         scope,
		MetadataKey:   key,
		MetadataValue: value,
	}, nil
}

// labelFor names a rule in an error: its name when it has one, otherwise its
// position in the file, so an unnamed rule is still identifiable.
func labelFor(index int, name string) string {
	if name != "" {
		return fmt.Sprintf("%q", name)
	}
	return fmt.Sprintf("rules[%d]", index)
}

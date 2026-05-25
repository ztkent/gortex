package analysis

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// GuardViolation describes a single guard rule violation.
type GuardViolation struct {
	RuleName    string `json:"rule_name"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	// Architecture-DSL fields. Populated only on layer / architecture
	// rule violations; empty on the flat co-change / boundary kinds.
	Violator  string `json:"violator,omitempty"`
	LayerFrom string `json:"layer_from,omitempty"`
	LayerTo   string `json:"layer_to,omitempty"`
	EdgeType  string `json:"edge_type,omitempty"`
}

// EvaluateGuards checks the given guard rules against a set of changed symbol IDs
// and the graph's edge structure, returning any violations found.
//
// For "co-change" rules: reports a violation when the change set contains symbols
// whose file paths match the rule's Source prefix but none matching the Target prefix.
//
// For "boundary" rules: reports a violation when any changed symbol whose file path
// matches the Source prefix has outgoing call or reference edges to symbols whose
// file paths match the Target prefix.
func EvaluateGuards(g graph.Store, rules []config.GuardRule, changedSymbolIDs []string) []GuardViolation {
	var violations []GuardViolation

	// Pre-resolve changed symbols to nodes for efficient lookup.
	changedNodes := make([]*graph.Node, 0, len(changedSymbolIDs))
	for _, id := range changedSymbolIDs {
		if n := g.GetNode(id); n != nil {
			changedNodes = append(changedNodes, n)
		}
	}

	for _, rule := range rules {
		switch rule.Kind {
		case "co-change":
			violations = append(violations, evaluateCoChange(rule, changedNodes)...)
		case "boundary":
			violations = append(violations, evaluateBoundary(g, rule, changedNodes)...)
		}
	}

	return violations
}

// evaluateCoChange checks whether the change set includes symbols from the source
// prefix but is missing symbols from the target prefix.
func evaluateCoChange(rule config.GuardRule, changedNodes []*graph.Node) []GuardViolation {
	hasSource := false
	hasTarget := false

	for _, n := range changedNodes {
		if strings.HasPrefix(n.FilePath, rule.Source) {
			hasSource = true
		}
		if strings.HasPrefix(n.FilePath, rule.Target) {
			hasTarget = true
		}
		if hasSource && hasTarget {
			return nil // both present, no violation
		}
	}

	if hasSource && !hasTarget {
		msg := rule.Message
		if msg == "" {
			msg = fmt.Sprintf("changes to %s require corresponding changes to %s", rule.Source, rule.Target)
		}
		return []GuardViolation{{
			RuleName:    rule.Name,
			Kind:        "co-change",
			Description: msg,
		}}
	}

	return nil
}

// evaluateBoundary checks whether any changed symbol in the source prefix has
// outgoing call or reference edges targeting symbols in the target prefix.
func evaluateBoundary(g graph.Store, rule config.GuardRule, changedNodes []*graph.Node) []GuardViolation {
	var violations []GuardViolation
	seen := make(map[string]bool)

	for _, n := range changedNodes {
		if !strings.HasPrefix(n.FilePath, rule.Source) {
			continue
		}

		outEdges := g.GetOutEdges(n.ID)
		for _, edge := range outEdges {
			if edge.Kind != graph.EdgeCalls && edge.Kind != graph.EdgeReferences {
				continue
			}

			target := g.GetNode(edge.To)
			if target == nil {
				continue
			}

			if !strings.HasPrefix(target.FilePath, rule.Target) {
				continue
			}

			// Deduplicate by source→target pair.
			key := n.ID + "->" + target.ID
			if seen[key] {
				continue
			}
			seen[key] = true

			msg := rule.Message
			if msg == "" {
				msg = fmt.Sprintf("%s must not directly reference %s", rule.Source, rule.Target)
			}

			violations = append(violations, GuardViolation{
				RuleName:    rule.Name,
				Kind:        "boundary",
				Description: fmt.Sprintf("%s: %s %s %s", msg, n.ID, edge.Kind, target.ID),
			})
		}
	}

	return violations
}

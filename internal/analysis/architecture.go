package analysis

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// EvaluateArchitecture checks the declarative architecture DSL — named
// layers with directional allow/deny constraints — against a set of
// changed symbols.
//
// For each changed symbol it resolves the symbol's layer, walks its
// outgoing call / reference edges, resolves each target's layer, and
// reports a violation when a cross-layer dependency breaks the source
// layer's allow/deny rules. Symbols in no declared layer, and edges
// to such symbols, are unconstrained.
func EvaluateArchitecture(g *graph.Graph, arch config.ArchitectureConfig, changedSymbolIDs []string) []GuardViolation {
	if g == nil || arch.IsEmpty() {
		return nil
	}
	names := sortedLayerNames(arch.Layers)

	var violations []GuardViolation
	seen := make(map[string]bool)
	for _, id := range changedSymbolIDs {
		n := g.GetNode(id)
		if n == nil {
			continue
		}
		fromLayer := layerOf(effectivePath(n), arch.Layers, names)
		if fromLayer == "" {
			continue
		}
		for _, e := range g.GetOutEdges(id) {
			if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
				continue
			}
			target := g.GetNode(e.To)
			if target == nil {
				continue
			}
			toLayer := layerOf(effectivePath(target), arch.Layers, names)
			if toLayer == "" || toLayer == fromLayer {
				continue
			}
			ok, reason := layerAllows(arch.Layers[fromLayer], fromLayer, toLayer)
			if ok {
				continue
			}
			key := id + "\x00" + e.To
			if seen[key] {
				continue
			}
			seen[key] = true
			violations = append(violations, GuardViolation{
				RuleName: "layer:" + fromLayer,
				Kind:     "layer",
				Description: fmt.Sprintf("%s (layer %s) %s %s (layer %s): %s",
					n.ID, fromLayer, e.Kind, target.ID, toLayer, reason),
				Violator:  n.ID,
				LayerFrom: fromLayer,
				LayerTo:   toLayer,
				EdgeType:  string(e.Kind),
			})
		}
	}
	return violations
}

// layerAllows reports whether a dependency from one layer to another
// is permitted, and the human-readable reason when it is not.
//
// Precedence: an explicit deny of the target layer always wins; then
// a non-empty Allow whitelist requires the target to be listed; with
// no whitelist a wildcard deny ("*") blocks every cross-layer edge.
func layerAllows(rule config.LayerRule, from, to string) (bool, string) {
	for _, d := range rule.Deny {
		if d == to {
			return false, fmt.Sprintf("layer %q denies dependencies on %q", from, to)
		}
	}
	if len(rule.Allow) > 0 {
		for _, a := range rule.Allow {
			if a == "*" || a == to {
				return true, ""
			}
		}
		return false, fmt.Sprintf("layer %q may depend only on %s, not %q",
			from, strings.Join(rule.Allow, ", "), to)
	}
	for _, d := range rule.Deny {
		if d == "*" {
			return false, fmt.Sprintf("layer %q denies all cross-layer dependencies", from)
		}
	}
	return true, ""
}

// layerOf returns the name of the layer a file belongs to, or "" when
// no layer claims it. names must be sorted so an ambiguous file
// (matched by two layers) resolves deterministically to the first.
func layerOf(filePath string, layers map[string]config.LayerRule, names []string) string {
	for _, name := range names {
		rule := layers[name]
		if len(rule.Paths) > 0 {
			for _, p := range rule.Paths {
				if globMatch(p, filePath) {
					return name
				}
			}
			continue
		}
		// A layer with no explicit paths claims files that carry the
		// layer name as a path segment — supports the terse config form.
		if pathHasSegment(filePath, name) {
			return name
		}
	}
	return ""
}

// sortedLayerNames returns the layer names in a stable order.
func sortedLayerNames(layers map[string]config.LayerRule) []string {
	names := make([]string, 0, len(layers))
	for name := range layers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// effectivePath strips the multi-repo prefix from a node's file path
// so per-repo architecture globs match in both single- and multi-repo
// graphs.
func effectivePath(n *graph.Node) string {
	if n.RepoPrefix != "" {
		return strings.TrimPrefix(n.FilePath, n.RepoPrefix+"/")
	}
	return n.FilePath
}

// pathHasSegment reports whether seg appears as a full path segment of
// filePath (e.g. seg "domain" matches "internal/domain/user.go").
func pathHasSegment(filePath, seg string) bool {
	for _, part := range strings.Split(filePath, "/") {
		if part == seg {
			return true
		}
	}
	return false
}

// globMatch reports whether path matches a glob pattern. "**" matches
// any number of path segments (including zero); "*" and "?" match
// within a single segment via the stdlib path.Match rules.
func globMatch(pattern, p string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchSegments(pat, seg []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(seg); i++ {
				if matchSegments(pat[1:], seg[i:]) {
					return true
				}
			}
			return false
		}
		if len(seg) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], seg[0]); err != nil || !ok {
			return false
		}
		pat, seg = pat[1:], seg[1:]
	}
	return len(seg) == 0
}

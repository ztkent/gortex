package languages

import (
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// goFlagMethods maps a flag-check method name to its inferred
// provider. The classification is best-effort: a domain function
// named IsEnabled would be misclassified as Unleash, but in
// practice every reasonable codebase that has its own IsEnabled
// either *is* a flag client (matching the spec exactly) or carries
// enough surrounding context that the false positive is acceptable
// noise the gate can be turned off to suppress.
//
// Provider strings match the spec's enumeration so downstream
// consumers (cleanup queries, drift detection) can scope by
// provider without parsing meta.
var goFlagMethods = map[string]string{
	// LaunchDarkly — typed Variation methods.
	"BoolVariation":   "launchdarkly",
	"StringVariation": "launchdarkly",
	"IntVariation":    "launchdarkly",
	"FloatVariation":  "launchdarkly",
	"JSONVariation":   "launchdarkly",
	// GrowthBook.
	"IsOn":    "growthbook",
	"Feature": "growthbook",
	// Unleash.
	"IsEnabled": "unleash",
	// Generic / internal flags packages — fall back to "internal".
	// The provider field on the resulting node carries the inferred
	// provider; the heuristic-based classification is good-enough
	// for the cleanup-workflow use case (find old flags) without
	// needing per-call type resolution.
}

// goFlagOpByMethod returns the operation kind ("read" / "write" /
// "register") for a recognised flag-check method. Today only reads
// are recognised — writes (toggling a flag from code) and
// registrations (declaring a new flag) are uncommon enough that
// they don't justify the per-method dispatch yet. Reserved for
// future expansion.
func goFlagOpByMethod(method string) string {
	_ = method
	return "read"
}

// goFlagEvent is the deferred record emitted at capture time and
// resolved during the post-pass. Mirrors goObservabilityEvent.
type goFlagEvent struct {
	provider string // launchdarkly / growthbook / unleash / internal
	method   string // exact method name
	name     string // flag identifier — first string-literal arg
	line     int    // 1-based line of the call expression
}

// detectGoFlagCheck inspects a callm.expr capture and returns the
// resolved provider plus flag name when the call matches the
// flag-method set and carries a string-literal flag identifier.
// ok=false on every other shape.
func detectGoFlagCheck(callExpr *sitter.Node, method string, src []byte) (provider, name string, ok bool) {
	if callExpr == nil {
		return "", "", false
	}
	provider, ok = goFlagMethods[method]
	if !ok {
		return "", "", false
	}
	args := callExpr.ChildByFieldName("arguments")
	if args == nil {
		return "", "", false
	}
	for i := 0; i < int(args.NamedChildCount()); i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() != "interpreted_string_literal" && c.Type() != "raw_string_literal" {
			continue
		}
		text := strings.Trim(c.Content(src), "\"`")
		if text == "" {
			return "", "", false
		}
		return provider, text, true
	}
	return "", "", false
}

// emitGoFlagChecks turns deferred flag-check records into KindFlag
// nodes and EdgeTogglesFlag edges. Nodes share IDs across files in
// a repo — the same flag name produces a single node per provider
// that every check site links to. graph.AddNode dedupes on ID, so
// emitting the same flag from multiple files in the same call is
// cheap.
//
// callerLookup maps a 1-based line to the enclosing function ID,
// matching the observability emitter's contract.
func emitGoFlagChecks(events []goFlagEvent, callerLookup func(line int) string, filePath string, result *parser.ExtractionResult) {
	if len(events) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(events))
	for _, e := range events {
		callerID := callerLookup(e.line)
		if callerID == "" {
			continue
		}
		flagID := goFlagNodeID(e.provider, e.name)
		if _, ok := seen[flagID]; !ok {
			seen[flagID] = struct{}{}
			result.Nodes = append(result.Nodes, &graph.Node{
				ID:       flagID,
				Kind:     graph.KindFlag,
				Name:     e.name,
				FilePath: filePath, // first sighting; not authoritative
				Language: "go",
				Meta: map[string]any{
					"provider": e.provider,
					"name":     e.name,
				},
			})
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     callerID,
			To:       flagID,
			Kind:     graph.EdgeTogglesFlag,
			FilePath: filePath,
			Line:     e.line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"op":     goFlagOpByMethod(e.method),
				"method": e.method,
			},
		})
	}
}

// goFlagNodeID is the canonical ID for a feature-flag node. The
// `flag::` prefix is reserved for shared flag identifiers and
// matches the synthetic-ID convention used by `module::`,
// `event::`, `external::`, and `annotation::` so the exporter
// surfaces it through the same stub-node code path.
func goFlagNodeID(provider, name string) string {
	if provider == "" {
		provider = "internal"
	}
	return "flag::" + provider + "::" + name
}

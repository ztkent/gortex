package analysis

import (
	"math"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
)

// DeadCodeEntry represents a symbol with zero incoming references that is not excluded.
type DeadCodeEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	FilePath string `json:"file_path"`
	Line     int    `json:"start_line"`
}

// HotspotEntry represents a symbol with disproportionately high complexity metrics.
type HotspotEntry struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	Kind               string  `json:"kind"`
	FilePath           string  `json:"file_path"`
	Line               int     `json:"start_line"`
	FanIn              int     `json:"fan_in"`
	FanOut             int     `json:"fan_out"`
	CommunityCrossings int     `json:"community_crossings"`
	// Betweenness is the node's betweenness-centrality score
	// normalized to 0-100 — how often it sits on a shortest path
	// between other symbols. A bottleneck the call graph routes
	// through scores high here even when its fan-in/out look modest.
	Betweenness     float64 `json:"betweenness"`
	ComplexityScore float64 `json:"complexity_score"`
}

// FindDeadCodeOptions controls filtering behavior for dead code analysis.
//
// Default behaviour ships only the high-signal kinds: function, method,
// type, interface. The opt-in flags below let callers pull in the
// lower-signal kinds (fields, variables, constants) that the graph can
// represent but can't reliably evaluate due to the absence of intra-
// function data-flow edges.
//
// Kinds that the dead-code analyzer never reports (regardless of flags):
// param, closure, generic_param, string, enum_member, module, column,
// table, config_key, flag, event, migration, fixture, todo, team,
// release, license, resource, kustomization, image, contract,
// file, package, import. These are either structural (file/package/
// import), extracted metadata (todo/team/release/license/fixture),
// infra (resource/kustomization/image/table/column/config_key/flag/
// event/migration), or function-shape (param/closure/generic_param)
// — none of them have a meaningful "is this code dead?" answer, and
// surfacing them drowns the real dead-function signal in noise.
type FindDeadCodeOptions struct {
	// IncludeVariables includes variable nodes in the results. Default false.
	// Variables are excluded by default because the graph does not track
	// intra-function data flow — local variables always appear "dead" even
	// though Go's compiler enforces their usage. Package-level variables
	// cannot be reliably distinguished from locals in the current graph model.
	IncludeVariables bool

	// IncludeFields includes struct/class field nodes in the results.
	// Default false. Same graph limitation as variables: a field read
	// inside a function body is captured as EdgeReads on the field node,
	// but the analyzer can't tell a real "field never read" from "graph
	// doesn't see the read because the resolver couldn't pick a
	// candidate." Fields are opt-in for callers that have manually
	// audited their resolver coverage.
	IncludeFields bool

	// IncludeConstants includes constant nodes (Go const, language
	// constants). Default false — same rationale as variables; the
	// graph can't distinguish "unused constant" from "constant read
	// inside a function body the resolver couldn't trace."
	IncludeConstants bool

	// IncludeCgoExports includes functions annotated with //export pragma.
	// Default false — CGo-exported functions are called from C, not Go,
	// so they have no incoming Go-level edges.
	// Requires the Go extractor to populate Node.Meta["cgo_export"] = true.
	IncludeCgoExports bool

	// IncludeLinknameTargets includes functions annotated with //go:linkname.
	// Default false — linkname targets are linked by name from another package
	// and have no visible call edges in the graph.
	// Requires the Go extractor to populate Node.Meta["go_linkname"] = true.
	IncludeLinknameTargets bool

	// SkipCrossRepoNodes excludes nodes whose RepoPrefix is non-empty.
	// Useful when cross-repo linking is incomplete — functions in secondary
	// repos may lack incoming edges from the primary repo.
	SkipCrossRepoNodes bool
}

// neverDeadCodeKinds enumerates node kinds the dead-code analyzer must
// never report — regardless of opt-in flags — because the question
// "is this code dead?" has no meaningful answer for them. Includes
// structural nodes (file/package/import), function-shape nodes
// (param/closure/generic_param), extracted metadata (todo/team/
// release/license), infra surface (table/column/migration/config_key/
// flag/event/fixture/resource/kustomization/image), package metadata
// (module), and value-extraction nodes (string/enum_member).
// Surfacing any of these drowns real dead-function signal in noise.
var neverDeadCodeKinds = map[graph.NodeKind]bool{
	graph.KindFile:          true,
	graph.KindPackage:       true,
	graph.KindImport:        true,
	graph.KindParam:         true,
	graph.KindClosure:       true,
	graph.KindGenericParam:  true,
	graph.KindString:        true,
	graph.KindEnumMember:    true,
	graph.KindModule:        true,
	graph.KindColumn:        true,
	graph.KindTable:         true,
	graph.KindConfigKey:     true,
	graph.KindFlag:          true,
	graph.KindEvent:         true,
	graph.KindMigration:     true,
	graph.KindFixture:       true,
	graph.KindTodo:          true,
	graph.KindTeam:          true,
	graph.KindRelease:       true,
	graph.KindLicense:       true,
	graph.KindResource:      true,
	graph.KindKustomization: true,
	graph.KindImage:         true,
	graph.KindContract:      true,
}

// incomingUsageKinds returns the set of incoming edge kinds that count
// as "this symbol is used" for the given node kind. The per-kind list
// matters because different shapes are exercised by different edges:
// a function is used via Calls or References, a type via References /
// Instantiates / MemberOf, a field via Reads or Writes.
//
// Before this split, the analyzer used a single global allowlist
// {Calls, References, MemberOf, Implements, Instantiates} — which
// meant struct fields and variables always appeared dead because
// the resolver records their use as EdgeReads, which wasn't in the
// allowlist. The result was 5,390 fields flagged across the gortex
// workspace, drowning out the ~300 real function-level signals.
func incomingUsageKinds(k graph.NodeKind) []graph.EdgeKind {
	switch k {
	case graph.KindFunction:
		// Calls: invoked as `foo()`. References: passed as a value
		// (`RunE: runClean`). MemberOf: appears in a method-table /
		// receiver mapping. Instantiates: NewFoo() pattern when the
		// receiver type is the function type itself.
		return []graph.EdgeKind{
			graph.EdgeCalls, graph.EdgeReferences,
			graph.EdgeMemberOf, graph.EdgeInstantiates,
		}
	case graph.KindMethod:
		// Same as functions plus: Implements (the method satisfies
		// an interface contract — required by the interface).
		return []graph.EdgeKind{
			graph.EdgeCalls, graph.EdgeReferences,
			graph.EdgeMemberOf, graph.EdgeImplements, graph.EdgeInstantiates,
		}
	case graph.KindType, graph.KindInterface:
		// Types are exercised by References (generic value-position
		// use), Instantiates (struct literal), MemberOf (methods/
		// fields hanging off the type), Implements (a type satisfies
		// this interface), Extends (subclass), Composes (embeds),
		// TypedAs (variable / param / field declared as this type),
		// Returns (function returns this type — the canonical pattern
		// for cross-package type re-export via `type X = pkg.X`).
		return []graph.EdgeKind{
			graph.EdgeReferences, graph.EdgeInstantiates,
			graph.EdgeMemberOf, graph.EdgeImplements,
			graph.EdgeExtends, graph.EdgeComposes,
			graph.EdgeTypedAs, graph.EdgeReturns,
		}
	case graph.KindField:
		// Fields are accessed via Reads/Writes (the dominant pattern)
		// and References (when a struct literal positionally fills the
		// field). MemberOf isn't a "use" — it just attaches the field
		// to its owner type.
		return []graph.EdgeKind{
			graph.EdgeReads, graph.EdgeWrites, graph.EdgeReferences,
		}
	case graph.KindVariable, graph.KindConstant:
		// Same as fields: Reads/Writes dominate; References covers
		// the value-as-arg case.
		return []graph.EdgeKind{
			graph.EdgeReads, graph.EdgeWrites, graph.EdgeReferences,
		}
	}
	// Fallback for any kind not specifically modelled: use the legacy
	// global allowlist so a future KindWidget doesn't silently
	// collapse to "always dead."
	return []graph.EdgeKind{
		graph.EdgeCalls, graph.EdgeReferences,
		graph.EdgeMemberOf, graph.EdgeImplements, graph.EdgeInstantiates,
	}
}

// isEntryPointNode reports whether n was stamped as a framework entry
// point (Alembic / Next.js / ASP.NET) by the entrypoints detector.
func isEntryPointNode(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	v, _ := n.Meta["entry_point"].(bool)
	return v
}

// FindDeadCode returns all symbols with zero incoming calls or references,
// excluding entry points, test functions, exported symbols, and user-excluded patterns.
// By default, variables are excluded (see FindDeadCodeOptions for rationale).
func FindDeadCode(g graph.Store, processes *ProcessResult, excludePatterns []string, opts ...FindDeadCodeOptions) []DeadCodeEntry {
	var opt FindDeadCodeOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	nodes := g.AllNodes()
	// Build set of interface-required method names per type.
	// If a type implements an interface, all methods that the interface
	// requires are alive even if never called directly (they satisfy the
	// contract).  We index: typeID → set of required method names.
	// Only EdgeImplements is needed — pulling AllEdges over cgo was
	// the previous OOM source (a ~300k-edge workspace materialises ~100
	// MB of Edge structs).
	ifaceRequiredMethods := buildIfaceRequiredMethods(g, nodes)

	// Build set of entry point node IDs from processes
	entryPoints := make(map[string]bool)
	if processes != nil {
		for _, proc := range processes.Processes {
			entryPoints[proc.EntryPoint] = true
			// Also consider all nodes that participate in any process
			for _, step := range proc.Steps {
				entryPoints[step.ID] = true
			}
		}
	}

	// Files holding a framework entry point (Alembic migrations,
	// Next.js pages, ASP.NET host files) — every symbol inside is
	// reachable from a runtime, not application-dead.
	entryPointFiles := make(map[string]bool)
	for _, n := range nodes {
		if n.Kind == graph.KindFile && isEntryPointNode(n) {
			entryPointFiles[n.FilePath] = true
		}
	}

	// Batched in-edge fetch for every node up front. The legacy per-node
	// g.GetInEdges(n.ID) call inside the main loop fired one Cypher per
	// node on Ladybug — ~133k cgo round-trips on the gortex workspace,
	// ~130s wall-clock, RSS spike that OOM-killed the daemon mid-pass.
	// GetInEdgesByNodeIDs collapses that to a single backend round-trip
	// keyed on the candidate id set.
	nodeIDs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		nodeIDs = append(nodeIDs, n.ID)
	}
	inEdgesByID := g.GetInEdgesByNodeIDs(nodeIDs)

	var result []DeadCodeEntry
	for _, n := range nodes {
		// Skip kinds the analyzer never reports — structural,
		// extracted metadata, infra, function-shape, and value-only
		// nodes. See neverDeadCodeKinds for the full list and why.
		if neverDeadCodeKinds[n.Kind] {
			continue
		}

		// Framework entry points, and everything in an entry-point
		// file, are invoked by a runtime — never dead.
		if isEntryPointNode(n) || entryPointFiles[n.FilePath] {
			continue
		}

		// Skip variables/fields/constants unless explicitly opted in.
		// All three are subject to the same graph limitation: the
		// resolver can't always pick a candidate for intra-function
		// reads, so they look dead even when the code reads them
		// every line. We err toward false-negative (miss a real dead
		// variable) over false-positive (flag every struct field
		// in the repo) — the latter destroys the signal of the
		// function/method results we DO trust.
		if n.Kind == graph.KindVariable && !opt.IncludeVariables {
			continue
		}
		if n.Kind == graph.KindField && !opt.IncludeFields {
			continue
		}
		if n.Kind == graph.KindConstant && !opt.IncludeConstants {
			continue
		}

		// Skip implicitly-called constructors/initializers.
		// Go: init() is called by the runtime.
		// Python: __init__ is called when a class is instantiated.
		if n.Name == "init" && n.Language == "go" {
			continue
		}
		if n.Name == "__init__" && n.Language == "python" {
			continue
		}

		// Skip Go main() — it's the binary entry point, called by the runtime.
		// Constrained to KindFunction so (*Foo).main() methods are still checked.
		if n.Name == "main" && n.Language == "go" && n.Kind == graph.KindFunction {
			continue
		}

		// Skip vendored/generated C header functions — they're used via C
		// macros and linker symbols, invisible to the graph.
		if isVendoredOrGenerated(n.FilePath) {
			continue
		}

		// Skip functions in Go files with build constraints — only one
		// variant is active per build, so the others always look "dead".
		if n.Language == "go" && hasBuildConstraint(n.FilePath) {
			continue
		}

		// Count incoming edges that indicate the symbol is used.
		// The allowlist is per-kind: fields/variables/constants are
		// exercised by Reads/Writes; functions/methods by Calls/
		// References; types by References/Instantiates/MemberOf/
		// Implements/Extends/Composes/TypedAs. See incomingUsageKinds
		// for the rationale.
		//
		// Edges are pulled once below in inEdgesByID before the loop —
		// the original per-iteration GetInEdges(n.ID) call costs ~1 ms
		// of cgo round-trip per node on Ladybug, so on a 133k-node
		// workspace it was the 130-second loop that OOM-killed the
		// daemon. The batched fetch collapses that to a single Cypher
		// keyed on the surviving candidate ids.
		allowed := incomingUsageKinds(n.Kind)
		inEdges := inEdgesByID[n.ID]
		incomingCount := 0
		for _, e := range inEdges {
			for _, k := range allowed {
				if e.Kind == k {
					incomingCount++
					break
				}
			}
		}

		if incomingCount > 0 {
			continue
		}

		// For methods with zero incoming edges, check if they exist to satisfy
		// an interface contract.  Look up the receiver type via member_of edges
		// and check if any implemented interface requires this method name.
		if n.Kind == graph.KindMethod {
			outEdges := g.GetOutEdges(n.ID)
			for _, e := range outEdges {
				if e.Kind == graph.EdgeMemberOf {
					if required, ok := ifaceRequiredMethods[e.To]; ok {
						if required[n.Name] {
							incomingCount++ // treat as alive
							break
						}
					}
				}
			}
			if incomingCount > 0 {
				continue
			}

			// Fallback: well-known standard-library interface methods.
			// If the implements edge wasn't inferred, methods like ServeHTTP,
			// MarshalJSON, String, etc. are still almost certainly alive.
			if isWellKnownInterfaceMethod(n.Name, n.Language) {
				continue
			}
		}

		// Skip CGo-exported functions (called from C, no Go-level callers).
		if n.Language == "go" && !opt.IncludeCgoExports {
			if cgoExport, ok := n.Meta["cgo_export"].(bool); ok && cgoExport {
				continue
			}
		}

		// Skip go:linkname targets (linked by name from another package).
		if n.Language == "go" && !opt.IncludeLinknameTargets {
			if linkname, ok := n.Meta["go_linkname"].(bool); ok && linkname {
				continue
			}
		}

		// Skip nodes from secondary repos when cross-repo linking is incomplete.
		if opt.SkipCrossRepoNodes && n.RepoPrefix != "" {
			continue
		}

		// Check exclusions
		if entryPoints[n.ID] {
			continue
		}
		if isTestFilePath(n.FilePath) {
			continue
		}
		if isExportedSymbol(n.Name, n.Language) && !isPackagePrivateByConvention(n.FilePath, n.Language) {
			continue
		}
		if matchesExcludePattern(n.FilePath, n.ID, excludePatterns) {
			continue
		}

		result = append(result, DeadCodeEntry{
			ID:       n.ID,
			Name:     n.Name,
			Kind:     string(n.Kind),
			FilePath: n.FilePath,
			Line:     n.StartLine,
		})
	}

	// Sort by file path then line for deterministic output
	sort.Slice(result, func(i, j int) bool {
		if result[i].FilePath != result[j].FilePath {
			return result[i].FilePath < result[j].FilePath
		}
		return result[i].Line < result[j].Line
	})

	return result
}

// buildIfaceRequiredMethods returns a map from type ID → set of method names
// that the type must implement to satisfy its interfaces.  This is computed by:
//  1. Collecting all interfaces with their required method names (from Meta["methods"]).
//  2. Collecting all EdgeImplements edges (type → interface).
//  3. For each type that implements an interface, merging all required method names.
func buildIfaceRequiredMethods(g graph.Store, nodes []*graph.Node) map[string]map[string]bool {
	// Step 1: interface ID → required method names
	ifaceMethods := make(map[string]map[string]bool)
	for _, n := range nodes {
		if n.Kind != graph.KindInterface || n.Meta == nil {
			continue
		}
		raw, ok := n.Meta["methods"]
		if !ok {
			continue
		}
		methods := make(map[string]bool)
		switch v := raw.(type) {
		case []string:
			for _, m := range v {
				methods[m] = true
			}
		case []any:
			for _, m := range v {
				if s, ok := m.(string); ok {
					methods[s] = true
				}
			}
		}
		if len(methods) > 0 {
			ifaceMethods[n.ID] = methods
		}
	}

	if len(ifaceMethods) == 0 {
		return nil
	}

	// Step 2: type ID → set of required method names (from all implemented
	// interfaces). Only EdgeImplements is needed — stream it via
	// EdgesByKind so on disk backends (Ladybug) we issue a single Cypher
	// MATCH for that kind instead of pulling every edge in the graph and
	// filtering in Go. The pre-batched-iterator AllEdges() pull was the
	// OOM source on the analyze(dead_code) hot path: ~300k edges × ~kb
	// per Edge struct = enough sustained allocation to get the daemon
	// killed before the iteration ever started.
	result := make(map[string]map[string]bool)
	for e := range g.EdgesByKind(graph.EdgeImplements) {
		// EdgeImplements: From=type, To=interface
		iface, ok := ifaceMethods[e.To]
		if !ok {
			continue
		}
		if result[e.From] == nil {
			result[e.From] = make(map[string]bool)
		}
		for m := range iface {
			result[e.From][m] = true
		}
	}

	return result
}

// hotspotBetweennessWeight scales the betweenness component of a
// hotspot's raw score. Betweenness arrives normalized to 0-100 (same
// range as the fan-in/out/crossing terms after their own
// normalization is implicit), so a weight of 0.4 lets a pure
// bottleneck — a symbol every call path routes through — register as
// a hotspot without overpowering the fan-in/out signals that still
// dominate the ranking.
const hotspotBetweennessWeight = 0.4

// FindHotspots returns symbols whose ComplexityScore exceeds the given threshold.
// ComplexityScore = (fan_in * 2) + (fan_out * 1.5) + (community_crossings * 3) +
// (betweenness * hotspotBetweennessWeight), normalized to 0-100. Betweenness is a
// centrality component — how often the symbol lies on a shortest path between
// other symbols — that augments the fan-in/out signals rather than replacing them.
// If threshold <= 0, the default threshold is mean + 2*stddev.
func FindHotspots(g graph.Store, communities *CommunityResult, threshold float64) []HotspotEntry {
	nodes := g.AllNodes()
	edges := g.AllEdges()

	// Build lookup maps for community membership
	nodeToComm := make(map[string]string)
	if communities != nil {
		nodeToComm = communities.NodeToComm
	}

	// Build edge maps for fan-in and fan-out computation
	// fan_in: incoming calls + references
	// fan_out: outgoing calls
	fanIn := make(map[string]int)
	fanOut := make(map[string]int)

	for _, e := range edges {
		if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences {
			fanIn[e.To]++
		}
		if e.Kind == graph.EdgeCalls {
			fanOut[e.From]++
		}
	}

	// Compute community crossings per node: outgoing edges to nodes in different communities
	crossings := make(map[string]int)
	for _, e := range edges {
		if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences {
			fromComm := nodeToComm[e.From]
			toComm := nodeToComm[e.To]
			if fromComm != "" && toComm != "" && fromComm != toComm {
				crossings[e.From]++
			}
		}
	}

	// Betweenness centrality — exact on small graphs, sampled on
	// large ones. Normalized to 0-100 against the graph's own max so
	// it sits on the same scale as the other score terms.
	bc := ComputeBetweenness(g)
	betweenness := make(map[string]float64, len(bc.Scores))
	if bc.Max > 0 {
		for id, v := range bc.Scores {
			betweenness[id] = (v / bc.Max) * 100.0
		}
	}

	// Compute raw scores for function/method nodes only
	type rawEntry struct {
		node        *graph.Node
		fanIn       int
		fanOut      int
		crossing    int
		betweenness float64
		rawScore    float64
	}

	var entries []rawEntry
	for _, n := range nodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}

		fi := fanIn[n.ID]
		fo := fanOut[n.ID]
		cc := crossings[n.ID]
		bw := betweenness[n.ID]
		raw := float64(fi)*2.0 + float64(fo)*1.5 + float64(cc)*3.0 + bw*hotspotBetweennessWeight

		entries = append(entries, rawEntry{
			node:        n,
			fanIn:       fi,
			fanOut:      fo,
			crossing:    cc,
			betweenness: bw,
			rawScore:    raw,
		})
	}

	if len(entries) == 0 {
		return nil
	}

	// Find max raw score for normalization
	maxRaw := 0.0
	for _, e := range entries {
		if e.rawScore > maxRaw {
			maxRaw = e.rawScore
		}
	}

	// Normalize to 0-100
	normalized := make([]float64, len(entries))
	for i, e := range entries {
		if maxRaw > 0 {
			normalized[i] = (e.rawScore / maxRaw) * 100.0
		}
	}

	// Compute default threshold if not specified: mean + 2*stddev
	if threshold <= 0 {
		var sum float64
		for _, s := range normalized {
			sum += s
		}
		mean := sum / float64(len(normalized))

		var variance float64
		for _, s := range normalized {
			diff := s - mean
			variance += diff * diff
		}
		variance /= float64(len(normalized))
		stddev := math.Sqrt(variance)

		threshold = mean + 2.0*stddev
	}

	// Filter and build result
	var result []HotspotEntry
	for i, e := range entries {
		score := math.Round(normalized[i]*100) / 100 // round to 2 decimal places
		if score < threshold {
			continue
		}

		result = append(result, HotspotEntry{
			ID:                 e.node.ID,
			Name:               e.node.Name,
			Kind:               string(e.node.Kind),
			FilePath:           e.node.FilePath,
			Line:               e.node.StartLine,
			FanIn:              e.fanIn,
			FanOut:             e.fanOut,
			CommunityCrossings: e.crossing,
			Betweenness:        math.Round(e.betweenness*100) / 100,
			ComplexityScore:    score,
		})
	}

	// Sort by ComplexityScore descending
	sort.Slice(result, func(i, j int) bool {
		return result[i].ComplexityScore > result[j].ComplexityScore
	})

	return result
}

// isTestFilePath checks if a file path indicates a test file.
func isTestFilePath(path string) bool {
	base := filepath.Base(path)
	return strings.Contains(base, "_test.") ||
		strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") ||
		strings.HasPrefix(base, "test_") ||
		strings.Contains(path, "__tests__/")
}

// isPackagePrivateByConvention reports whether a file lives inside a
// directory the language's tooling treats as package-private regardless of
// individual symbol capitalisation. The dead-code analyzer uses this to
// override the "skip all exported symbols" rule: a function inside
// `gortex/internal/parser/tsitter/` named `Test` is *visible only to other
// gortex packages*, so if no caller exists in the indexed graph it really
// is dead — there's nowhere else it could be called from.
//
// Currently handles Go's `/internal/` convention (compiler-enforced since
// Go 1.4). Add other languages as their tooling acquires similar
// hard-bounded visibility rules.
func isPackagePrivateByConvention(filePath, lang string) bool {
	if lang != "go" {
		return false
	}
	// Match the path component "internal" anywhere in the path — Go's rule
	// is that anything inside an `internal/` directory is only importable
	// from its enclosing tree.
	return strings.Contains(filePath, "/internal/") ||
		strings.HasPrefix(filePath, "internal/")
}

// isExportedSymbol checks if a symbol name is exported (public API).
func isExportedSymbol(name, lang string) bool {
	if lang == "go" {
		if len(name) == 0 {
			return false
		}
		return unicode.IsUpper(rune(name[0]))
	}
	// For other languages, assume exported if not starting with underscore
	return len(name) > 0 && !strings.HasPrefix(name, "_")
}

// goWellKnownMethods contains method names that satisfy standard-library or
// widely-used Go interfaces.  When an implements edge wasn't inferred, a method
// with one of these names is almost certainly alive via implicit interface
// satisfaction rather than truly dead.
var goWellKnownMethods = map[string]bool{
	// io interfaces
	"Read": true, "Write": true, "Close": true, "Flush": true,
	"Seek": true, "ReadAt": true, "WriteAt": true, "ReadFrom": true,
	"WriteTo": true, "ReadByte": true, "UnreadByte": true,
	"ReadRune": true, "UnreadRune": true, "WriteByte": true,
	"WriteString": true,
	// net/http
	"ServeHTTP": true, "RoundTrip": true,
	// encoding
	"MarshalJSON": true, "UnmarshalJSON": true,
	"MarshalXML": true, "UnmarshalXML": true,
	"MarshalText": true, "UnmarshalText": true,
	"MarshalBinary": true, "UnmarshalBinary": true,
	"MarshalYAML": true, "UnmarshalYAML": true,
	// fmt
	"String": true, "Error": true, "Format": true, "GoString": true,
	// sort
	"Len": true, "Less": true, "Swap": true,
	// sql
	"Scan": true, "Value": true,
	// hash
	"Sum": true, "Reset": true, "BlockSize": true,
	// driver
	"Open": true, "Exec": true, "Query": true, "Begin": true,
	"Prepare": true,
	// proto/gRPC
	"mustEmbedUnimplemented": true, "ProtoMessage": true,
	"ProtoReflect": true,
}

// isWellKnownInterfaceMethod returns true if the method name matches a
// standard-library or widely-used interface method in the given language.
func isWellKnownInterfaceMethod(name, lang string) bool {
	if lang != "go" {
		return false
	}
	return goWellKnownMethods[name]
}

// isVendoredOrGenerated checks if a file is vendored or generated code that
// should be excluded from dead code analysis.
func isVendoredOrGenerated(path string) bool {
	if strings.Contains(path, "tree_sitter/") ||
		strings.Contains(path, "vendor/") ||
		strings.HasSuffix(path, ".h") ||
		strings.HasSuffix(path, ".c") {
		return true
	}
	base := filepath.Base(path)
	// Protobuf / gRPC generated Go files
	if strings.HasSuffix(base, ".pb.go") {
		return true
	}
	// Code-generation convention suffixes
	if strings.HasSuffix(base, "_gen.go") ||
		strings.HasSuffix(base, "_generated.go") ||
		strings.HasSuffix(base, ".gen.go") {
		return true
	}
	// controller-gen / kubebuilder: zz_generated.*.go
	if strings.HasPrefix(base, "zz_generated") {
		return true
	}
	// Mock files (mockery, gomock)
	if strings.HasPrefix(base, "mock_") && strings.HasSuffix(base, ".go") {
		return true
	}
	if strings.HasSuffix(base, "_mock.go") {
		return true
	}
	return false
}

// buildConstraintSuffixes covers OS, architecture, and special build-tag
// suffixes used by the Go toolchain for conditional compilation.
var buildConstraintSuffixes = []string{
	// OS
	"_linux.go", "_darwin.go", "_windows.go", "_freebsd.go",
	"_openbsd.go", "_netbsd.go", "_dragonfly.go", "_plan9.go",
	"_solaris.go", "_illumos.go", "_aix.go", "_android.go",
	"_ios.go", "_js.go", "_wasip1.go",
	// Architecture
	"_amd64.go", "_arm64.go", "_arm.go", "_386.go",
	"_mips.go", "_mipsle.go", "_mips64.go", "_mips64le.go",
	"_ppc64.go", "_ppc64le.go", "_s390x.go", "_riscv64.go",
	"_loong64.go", "_wasm.go",
	// Special
	"_stub.go", "_cgo.go", "_nocgo.go", "_purego.go", "_appengine.go",
}

// hasBuildConstraint checks if a Go file has build constraints (build tags).
// Files with build constraints are conditionally compiled — only one variant
// is active per build, so inactive variants always look "dead".
func hasBuildConstraint(path string) bool {
	base := filepath.Base(path)
	for _, s := range buildConstraintSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	return false
}

// matchesExcludePattern checks if a node matches any user-configured exclusion pattern.
// Patterns are matched against both the file path and the node ID.
func matchesExcludePattern(filePath, nodeID string, patterns []string) bool {
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		// Try glob match against file path
		if matched, _ := filepath.Match(pattern, filePath); matched {
			return true
		}
		// Try prefix match against file path
		if strings.HasPrefix(filePath, pattern) {
			return true
		}
		// Try prefix match against node ID
		if strings.HasPrefix(nodeID, pattern) {
			return true
		}
	}
	return false
}

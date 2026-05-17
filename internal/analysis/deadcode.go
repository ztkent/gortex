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
	ComplexityScore    float64 `json:"complexity_score"`
}

// FindDeadCodeOptions controls filtering behavior for dead code analysis.
type FindDeadCodeOptions struct {
	// IncludeVariables includes variable nodes in the results. Default false.
	// Variables are excluded by default because the graph does not track
	// intra-function data flow — local variables always appear "dead" even
	// though Go's compiler enforces their usage. Package-level variables
	// cannot be reliably distinguished from locals in the current graph model.
	IncludeVariables bool

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

// FindDeadCode returns all symbols with zero incoming calls or references,
// excluding entry points, test functions, exported symbols, and user-excluded patterns.
// By default, variables are excluded (see FindDeadCodeOptions for rationale).
func FindDeadCode(g *graph.Graph, processes *ProcessResult, excludePatterns []string, opts ...FindDeadCodeOptions) []DeadCodeEntry {
	var opt FindDeadCodeOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	nodes := g.AllNodes()
	allEdges := g.AllEdges()

	// Build set of interface-required method names per type.
	// If a type implements an interface, all methods that the interface
	// requires are alive even if never called directly (they satisfy the
	// contract).  We index: typeID → set of required method names.
	ifaceRequiredMethods := buildIfaceRequiredMethods(g, nodes, allEdges)

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

	var result []DeadCodeEntry
	for _, n := range nodes {
		// Skip structural node kinds
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport || n.Kind == graph.KindPackage {
			continue
		}

		// Skip variables unless explicitly requested — the graph lacks
		// intra-function data-flow edges, so variables always look "dead".
		if n.Kind == graph.KindVariable && !opt.IncludeVariables {
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
		// Besides calls and references, we also count:
		//   - member_of:     methods point to this type → the type is used
		//   - implements:    a type implements this interface → the interface is used
		//   - instantiates:  struct literal usage → the type is used
		inEdges := g.GetInEdges(n.ID)
		incomingCount := 0
		for _, e := range inEdges {
			switch e.Kind {
			case graph.EdgeCalls, graph.EdgeReferences,
				graph.EdgeMemberOf, graph.EdgeImplements, graph.EdgeInstantiates:
				incomingCount++
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
func buildIfaceRequiredMethods(g *graph.Graph, nodes []*graph.Node, edges []*graph.Edge) map[string]map[string]bool {
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

	// Step 2: type ID → set of required method names (from all implemented interfaces)
	result := make(map[string]map[string]bool)
	for _, e := range edges {
		if e.Kind != graph.EdgeImplements {
			continue
		}
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

// FindHotspots returns symbols whose ComplexityScore exceeds the given threshold.
// ComplexityScore = (fan_in * 2) + (fan_out * 1.5) + (community_crossings * 3), normalized to 0-100.
// If threshold <= 0, the default threshold is mean + 2*stddev.
func FindHotspots(g *graph.Graph, communities *CommunityResult, threshold float64) []HotspotEntry {
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

	// Compute raw scores for function/method nodes only
	type rawEntry struct {
		node     *graph.Node
		fanIn    int
		fanOut   int
		crossing int
		rawScore float64
	}

	var entries []rawEntry
	for _, n := range nodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}

		fi := fanIn[n.ID]
		fo := fanOut[n.ID]
		cc := crossings[n.ID]
		raw := float64(fi)*2.0 + float64(fo)*1.5 + float64(cc)*3.0

		entries = append(entries, rawEntry{
			node:     n,
			fanIn:    fi,
			fanOut:   fo,
			crossing: cc,
			rawScore: raw,
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

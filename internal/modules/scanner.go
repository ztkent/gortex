// Package modules parses dependency-manifest files and emits
// KindModule nodes plus EdgeDependsOnModule edges so agents can
// answer "what external packages does this repo depend on" or
// "which files import lodash@4" with a single graph query.
//
// Scope (v1): Go's go.mod. Other ecosystems (package.json, pnpm-
// lock, requirements.txt, Cargo.toml, etc.) are tracked as future
// follow-ups; the scanner's API is shaped so they can land
// alongside without changing the call sites.
package modules

import (
	"bufio"
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/zzet/gortex/internal/graph"
)

// Spec is one parsed dependency entry from a manifest file.
// Indirect is true for entries marked `// indirect` in go.mod —
// agents may want to scope queries to direct deps only, so the
// flag rides along on the graph node's meta.
type Spec struct {
	Ecosystem string // "go", "npm", "pypi", … — for v1 always "go"
	Path      string // module path / package name
	Version   string // version string, "" for unpinned
	Indirect  bool
	Replace   string // replacement path when go.mod has a `replace` directive, "" otherwise
	Line      int    // 1-based line in the manifest where the spec was found
}

// ParseGoMod walks go.mod source and returns one Spec per
// dependency. Handles three shapes:
//
//	require github.com/foo/bar v1.0.0          // single-line
//	require ( ... )                            // grouped block
//	replace github.com/foo/bar => ./local/bar  // local replacements
//
// `// indirect` markers attach to the relevant Spec. Comments and
// blank lines are skipped. Errors silently produce a partial Spec
// list — the indexer treats malformed go.mod as best-effort, not
// fatal.
func ParseGoMod(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	var specs []Spec
	scanner := bufio.NewScanner(bytes.NewReader(source))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	inRequire := false
	inReplace := false
	lineNum := 0
	replaces := map[string]string{}
	for scanner.Scan() {
		lineNum++
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		// Block markers.
		switch {
		case strings.HasPrefix(line, "require ("):
			inRequire = true
			continue
		case strings.HasPrefix(line, "replace ("):
			inReplace = true
			continue
		case line == ")":
			inRequire = false
			inReplace = false
			continue
		}
		// Replace directives — collect first so we can stamp the
		// replacement onto the require Spec produced from the same
		// module path. Single-line and block forms both supported.
		if strings.HasPrefix(line, "replace ") || inReplace {
			from, to := parseReplace(line)
			if from != "" {
				replaces[from] = to
			}
			continue
		}
		// Require directives.
		var modulePath, version string
		var directiveLine = lineNum
		switch {
		case strings.HasPrefix(line, "require ") && !strings.Contains(line, "("):
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				modulePath = parts[1]
				version = parts[2]
			}
		case inRequire:
			parts := strings.Fields(line)
			if len(parts) >= 2 && strings.Contains(parts[0], "/") {
				modulePath = parts[0]
				version = parts[1]
			}
		}
		if modulePath == "" {
			continue
		}
		spec := Spec{
			Ecosystem: "go",
			Path:      modulePath,
			Version:   version,
			Indirect:  strings.Contains(raw, "// indirect"),
			Line:      directiveLine,
		}
		specs = append(specs, spec)
	}
	// Stamp replace targets onto the matching require Spec — done
	// after the full pass so block-form replaces declared after
	// requires still attach correctly.
	for i := range specs {
		if to, ok := replaces[specs[i].Path]; ok {
			specs[i].Replace = to
		}
	}
	return specs
}

// ParsePackageJSON walks an npm-style package.json file's
// dependency blocks (`dependencies`, `devDependencies`,
// `peerDependencies`, `optionalDependencies`) and returns one
// Spec per declared package. The Indirect flag is repurposed for
// devDependencies/peerDependencies/optionalDependencies — they
// aren't "indirect" in the same Go-module sense, but the flag
// lets agents scope to "production-only" deps without a separate
// axis. The exact source block lives on Replace as a tag string
// (not the most semantically clean home, but it avoids growing
// the Spec struct for an ecosystem-specific field) — production
// deps get an empty Replace.
//
// Version strings are kept verbatim — npm semver ranges
// (`^1.2.0`, `~3.4.1`, `>=2 <3`) are not normalised, since
// resolved-versions belong in the lockfile, not package.json.
// A future package-lock.json / pnpm-lock.yaml extractor will
// supersede the version string with the resolved one.
func ParsePackageJSON(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	var manifest struct {
		Dependencies         map[string]string `json:"dependencies"`
		DevDependencies      map[string]string `json:"devDependencies"`
		PeerDependencies     map[string]string `json:"peerDependencies"`
		OptionalDependencies map[string]string `json:"optionalDependencies"`
	}
	if err := json.Unmarshal(source, &manifest); err != nil {
		return nil
	}
	var specs []Spec
	specs = append(specs, packageJSONBlock(manifest.Dependencies, "")...)
	specs = append(specs, packageJSONBlock(manifest.DevDependencies, "dev")...)
	specs = append(specs, packageJSONBlock(manifest.PeerDependencies, "peer")...)
	specs = append(specs, packageJSONBlock(manifest.OptionalDependencies, "optional")...)
	return specs
}

func packageJSONBlock(deps map[string]string, kind string) []Spec {
	if len(deps) == 0 {
		return nil
	}
	out := make([]Spec, 0, len(deps))
	for name, version := range deps {
		out = append(out, Spec{
			Ecosystem: "npm",
			Path:      name,
			Version:   version,
			Indirect:  kind != "",
			Replace:   kind, // dev/peer/optional, "" for production
		})
	}
	// Stable order per block — JSON map iteration is randomised, and
	// downstream consumers (BuildGraphArtifacts dedup, prefix-sort
	// in LinkImports) tolerate any order, but tests want
	// determinism.
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

// ParsePyProject walks a Python pyproject.toml file's dependency
// declarations and returns one Spec per declared package. Both
// PEP 621 (`[project] dependencies = ["pkg>=1.0", ...]`) and
// Poetry (`[tool.poetry.dependencies] pkg = "^1.0"`) shapes are
// recognised; optional-dependency groups (`[project.optional-
// dependencies]`) are surfaced with the group name in Replace
// (analogous to how npm dev/peer/optional reuse the field).
//
// Version constraints are kept verbatim — PEP 440 specifiers and
// Poetry's caret/tilde ranges aren't normalised; resolved
// versions live in the lockfile (poetry.lock / uv.lock /
// requirements.txt) which a future extractor will supersede.
func ParsePyProject(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	var manifest struct {
		Project struct {
			Dependencies         []string            `toml:"dependencies"`
			OptionalDependencies map[string][]string `toml:"optional-dependencies"`
		} `toml:"project"`
		Tool struct {
			Poetry struct {
				Dependencies    map[string]any `toml:"dependencies"`
				DevDependencies map[string]any `toml:"dev-dependencies"`
			} `toml:"poetry"`
		} `toml:"tool"`
	}
	if err := toml.Unmarshal(source, &manifest); err != nil {
		return nil
	}

	var specs []Spec
	// PEP 621 dependencies: list of `name<spec>` strings.
	for _, dep := range manifest.Project.Dependencies {
		name, version := splitPEP508(dep)
		if name == "" {
			continue
		}
		specs = append(specs, Spec{
			Ecosystem: "pypi",
			Path:      name,
			Version:   version,
		})
	}
	// PEP 621 optional groups: each group name lands on Replace so
	// agents can scope by group later. Sorted within each group for
	// determinism.
	groupNames := make([]string, 0, len(manifest.Project.OptionalDependencies))
	for g := range manifest.Project.OptionalDependencies {
		groupNames = append(groupNames, g)
	}
	sort.Strings(groupNames)
	for _, group := range groupNames {
		entries := manifest.Project.OptionalDependencies[group]
		groupSpecs := make([]Spec, 0, len(entries))
		for _, dep := range entries {
			name, version := splitPEP508(dep)
			if name == "" {
				continue
			}
			groupSpecs = append(groupSpecs, Spec{
				Ecosystem: "pypi",
				Path:      name,
				Version:   version,
				Indirect:  true,
				Replace:   group,
			})
		}
		sort.Slice(groupSpecs, func(i, j int) bool {
			return groupSpecs[i].Path < groupSpecs[j].Path
		})
		specs = append(specs, groupSpecs...)
	}
	// Poetry shape: name → spec (string or table). When a table is
	// given we take its `version` field, when a bare string we keep
	// it verbatim.
	specs = append(specs, poetryBlock(manifest.Tool.Poetry.Dependencies, "")...)
	specs = append(specs, poetryBlock(manifest.Tool.Poetry.DevDependencies, "dev")...)
	return specs
}

// poetryBlock converts a Poetry `[tool.poetry.*-dependencies]`
// table into Spec entries. Values can be either bare version
// strings or tables containing a `version` key (for
// extras-bearing entries like `requests = { version = "^2.0",
// extras = ["socks"] }`).
func poetryBlock(deps map[string]any, kind string) []Spec {
	if len(deps) == 0 {
		return nil
	}
	out := make([]Spec, 0, len(deps))
	for name, raw := range deps {
		if name == "python" {
			// `python = "^3.10"` is a Python interpreter
			// constraint, not a dependency. Skip.
			continue
		}
		var version string
		switch v := raw.(type) {
		case string:
			version = v
		case map[string]any:
			if s, ok := v["version"].(string); ok {
				version = s
			}
		}
		out = append(out, Spec{
			Ecosystem: "pypi",
			Path:      name,
			Version:   version,
			Indirect:  kind != "",
			Replace:   kind,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

// pep508Re strips a PEP 508 dependency string into name + version
// constraint. Examples: `requests>=2.0` → ("requests", ">=2.0"),
// `flask[async]==2.0.0` → ("flask", "==2.0.0"), `numpy` → ("numpy", "").
// Environment markers (`pkg; python_version<'3.9'`) and URL
// installs (`pkg @ https://...`) are dropped — they encode
// install-context, not the dependency identity our graph cares
// about.
var pep508Re = regexp.MustCompile(`^([A-Za-z0-9_.\-]+)`)

func splitPEP508(dep string) (name, version string) {
	dep = strings.TrimSpace(dep)
	// Drop environment markers and URL specs at the first ';' / ' @ '.
	if idx := strings.Index(dep, ";"); idx >= 0 {
		dep = strings.TrimSpace(dep[:idx])
	}
	if idx := strings.Index(dep, " @ "); idx >= 0 {
		dep = strings.TrimSpace(dep[:idx])
	}
	// Drop extras (the `[group]` suffix).
	if idx := strings.Index(dep, "["); idx >= 0 {
		end := strings.Index(dep, "]")
		if end < 0 {
			return "", ""
		}
		dep = strings.TrimSpace(dep[:idx]) + strings.TrimSpace(dep[end+1:])
	}
	m := pep508Re.FindString(dep)
	if m == "" {
		return "", ""
	}
	name = m
	version = strings.TrimSpace(dep[len(m):])
	return name, version
}

// ParseRequirementsTxt walks a pip requirements file and returns
// one Spec per non-comment, non-blank entry. Each line is parsed
// like a PEP 508 dependency; `-r other.txt` and `-e .` lines are
// ignored — they're install-mode directives, not dependency
// declarations.
//
// A future enhancement could chase `-r` includes recursively, but
// the first-order coverage from a single file is the
// 90th-percentile use case.
func ParseRequirementsTxt(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	var specs []Spec
	scanner := bufio.NewScanner(bytes.NewReader(source))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Drop trailing inline comment.
		if i := strings.Index(line, "#"); i > 0 {
			line = strings.TrimSpace(line[:i])
		}
		if strings.HasPrefix(line, "-") {
			// `-r`, `-e`, `--index-url`, etc. — skip.
			continue
		}
		name, version := splitPEP508(line)
		if name == "" {
			continue
		}
		specs = append(specs, Spec{
			Ecosystem: "pypi",
			Path:      name,
			Version:   version,
		})
	}
	return specs
}

// parseReplace extracts the from/to module paths from a replace
// directive line. Returns ("", "") when the line doesn't have the
// expected `from [version] => to [version]` shape. Replace
// versions are dropped — they don't add graph signal beyond what
// the require directive already carries.
func parseReplace(line string) (from, to string) {
	line = strings.TrimPrefix(line, "replace ")
	idx := strings.Index(line, "=>")
	if idx < 0 {
		return "", ""
	}
	left := strings.TrimSpace(line[:idx])
	right := strings.TrimSpace(line[idx+2:])
	if left == "" || right == "" {
		return "", ""
	}
	// Drop optional version on the from side (`module v1.x => target`).
	if parts := strings.Fields(left); len(parts) > 0 {
		left = parts[0]
	}
	if parts := strings.Fields(right); len(parts) > 0 {
		right = parts[0]
	}
	return left, right
}

// ModuleNodeID returns the canonical ID for a module node. The
// `module::` prefix is reserved for shared external-dependency
// nodes; the version is included so two repos that depend on
// `lodash@3` and `lodash@4` produce two distinct nodes that can be
// joined for "version skew" queries.
func ModuleNodeID(ecosystem, path, version string) string {
	id := "module::" + ecosystem + ":" + path
	if version != "" {
		id += "@" + version
	}
	return id
}

// BuildGraphArtifacts converts the parsed Spec list into
// (modules, edges) pairs. Modules are de-duplicated within the
// returned slice — graph.AddNode is idempotent on ID, so one node
// per (ecosystem, path, version) tuple is guaranteed even when the
// caller appends from multiple manifest files.
//
// filePath is the unprefixed manifest path (typically "go.mod").
// applyRepoPrefix downstream handles multi-repo namespacing for
// the file→module edge, but module IDs themselves do not get
// prefixed — the synthetic `module::` prefix matches the existing
// `external::` / `annotation::` convention the exporter already
// recognises.
func BuildGraphArtifacts(filePath string, specs []Spec) ([]*graph.Node, []*graph.Edge) {
	if len(specs) == 0 {
		return nil, nil
	}
	filePath = filepath.ToSlash(filePath)
	seen := make(map[string]struct{}, len(specs))
	nodes := make([]*graph.Node, 0, len(specs))
	edges := make([]*graph.Edge, 0, len(specs))
	for _, s := range specs {
		id := ModuleNodeID(s.Ecosystem, s.Path, s.Version)
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			meta := map[string]any{
				"ecosystem": s.Ecosystem,
				"path":      s.Path,
				"version":   s.Version,
				"indirect":  s.Indirect,
			}
			if s.Replace != "" {
				meta["replace"] = s.Replace
			}
			nodes = append(nodes, &graph.Node{
				ID:       id,
				Kind:     graph.KindModule,
				Name:     shortName(s.Path),
				FilePath: filePath,
				Language: "go",
				Meta:     meta,
			})
		}
		edges = append(edges, &graph.Edge{
			From:     filePath,
			To:       id,
			Kind:     graph.EdgeDependsOnModule,
			FilePath: filePath,
			Line:     s.Line,
			Origin:   graph.OriginASTResolved,
		})
	}
	return nodes, edges
}

// LinkImports walks every KindImport node in the graph and emits
// an EdgeDependsOnModule edge to the matching module node from the
// given Spec list. Matching is by longest path prefix — an import
// of `github.com/foo/bar/sub` resolves to the spec for
// `github.com/foo/bar` when no exact match exists. Returns the
// number of edges emitted.
//
// Imports of repo-internal packages (the indexed module's own
// path) are deliberately skipped — they aren't external
// dependencies. Multi-version imports (Go's `module/v2` shape)
// match the longest spec; a manifest declaring both `bar` and
// `bar/v2` will resolve `import bar/v2/sub` to the v2 spec.
func LinkImports(g *graph.Graph, specs []Spec, ownModulePath string) int {
	if g == nil || len(specs) == 0 {
		return 0
	}
	// Index specs by path for quick longest-prefix lookup. When two
	// specs share a path (shouldn't happen in a well-formed go.mod,
	// but guard against duplicates) the first wins — graph node
	// dedup later handles any concrete conflict.
	specByPath := make(map[string]Spec, len(specs))
	paths := make([]string, 0, len(specs))
	for _, s := range specs {
		if _, ok := specByPath[s.Path]; ok {
			continue
		}
		specByPath[s.Path] = s
		paths = append(paths, s.Path)
	}
	// Sort longest first so the prefix scan picks the most specific
	// match without an O(n²) probe.
	sort.Slice(paths, func(i, j int) bool {
		return len(paths[i]) > len(paths[j])
	})

	emitted := 0
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindImport {
			continue
		}
		importPath, _ := n.Meta["path"].(string)
		if importPath == "" {
			continue
		}
		if ownModulePath != "" && (importPath == ownModulePath ||
			strings.HasPrefix(importPath, ownModulePath+"/")) {
			continue
		}
		matched := matchLongestPrefix(importPath, paths)
		if matched == "" {
			continue
		}
		spec := specByPath[matched]
		moduleID := ModuleNodeID(spec.Ecosystem, spec.Path, spec.Version)
		g.AddEdge(&graph.Edge{
			From:     n.ID,
			To:       moduleID,
			Kind:     graph.EdgeDependsOnModule,
			FilePath: n.FilePath,
			Line:     n.StartLine,
			Origin:   graph.OriginASTResolved,
		})
		emitted++
	}
	return emitted
}

// matchLongestPrefix returns the longest path from candidates that
// matches importPath as either an exact match or a directory
// prefix. Candidates must already be sorted by descending length;
// the first hit wins.
func matchLongestPrefix(importPath string, candidates []string) string {
	for _, p := range candidates {
		if importPath == p {
			return p
		}
		if strings.HasPrefix(importPath, p+"/") {
			return p
		}
	}
	return ""
}

// shortName returns the last meaningful segment of a module path —
// useful for the Name field surfaced by Brief listings. Strips the
// `vN` major-version suffix when present (`github.com/foo/bar/v2`
// → `bar`, not `v2`).
func shortName(path string) string {
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	last := parts[len(parts)-1]
	if isMajorVersionSegment(last) && len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return last
}

func isMajorVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

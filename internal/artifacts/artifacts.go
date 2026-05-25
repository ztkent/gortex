// Package artifacts materialises the `.gortex.yaml::artifacts`
// manifest — non-code knowledge files such as DB schemas, API specs,
// infra configs, and architecture docs — into first-class
// KindArtifact graph nodes linked by EdgeReferences to the symbols
// they mention.
//
// Artifacts are the structured slice of "context" the import graph
// never sees: the OpenAPI spec a handler implements, the SQL schema a
// model mirrors, the ADR that explains why a package exists. Tracking
// them as graph nodes lets an agent pull the right spec alongside the
// code via search_artifacts / get_artifact.
package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// minRefTokenLen is the shortest symbol name considered for reference
// matching. Shorter identifiers ("ID", "Get", "New") collide with
// ordinary prose and would link an artifact to half the graph.
const minRefTokenLen = 4

// maxScanBytes caps how much of a file is scanned for symbol
// references. The content hash still covers the whole file.
const maxScanBytes = 1 << 20 // 1 MiB

// maxRefsPerArtifact bounds the EdgeReferences fan-out of one
// artifact so a schema naming hundreds of types stays navigable.
const maxRefsPerArtifact = 200

// Artifact is one materialised knowledge file.
type Artifact struct {
	ID          string   `json:"id"`           // graph node ID — artifact::<path>
	Path        string   `json:"path"`         // repo-relative file path
	Name        string   `json:"name"`         // display name
	Kind        string   `json:"kind"`         // schema | api | infra | doc
	ContentHash string   `json:"content_hash"` // sha256 hex of the file
	Size        int      `json:"size"`         // byte length
	References  []string `json:"references"`   // symbol node IDs the artifact mentions
}

// Materialize reads the configured artifact files under root, builds
// KindArtifact nodes, and links each to the symbols it mentions via
// EdgeReferences. Returns the artifacts materialised, sorted by path.
//
// repoPrefix scopes node IDs / paths in a multi-repo graph; pass ""
// for a single-repo graph. Best-effort — missing or unreadable files
// are skipped rather than failing the whole pass.
func Materialize(g graph.Store, root string, entries []config.ArtifactEntry, repoPrefix string) []Artifact {
	if g == nil || root == "" || len(entries) == 0 {
		return nil
	}
	nameIndex := buildSymbolIndex(g, repoPrefix)

	seen := make(map[string]bool)
	var out []Artifact
	for _, entry := range entries {
		for _, rel := range expandGlob(root, entry.Path) {
			if seen[rel] {
				continue
			}
			seen[rel] = true
			art, ok := materializeOne(g, root, rel, entry, repoPrefix, nameIndex)
			if ok {
				out = append(out, art)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// materializeOne reads one artifact file and projects it onto the graph.
func materializeOne(g graph.Store, root, rel string, entry config.ArtifactEntry, repoPrefix string, nameIndex map[string][]string) (Artifact, bool) {
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return Artifact{}, false
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	kind := strings.TrimSpace(entry.Kind)
	if kind == "" {
		kind = detectKind(rel)
	}
	name := strings.TrimSpace(entry.Name)
	if name == "" {
		name = path.Base(rel)
	}

	graphPath := rel
	if repoPrefix != "" {
		graphPath = repoPrefix + "/" + rel
	}
	nodeID := "artifact::" + graphPath

	refs := scanReferences(data, nameIndex)

	node := &graph.Node{
		ID:         nodeID,
		Kind:       graph.KindArtifact,
		Name:       name,
		FilePath:   graphPath,
		RepoPrefix: repoPrefix,
		Meta: map[string]any{
			"artifact_kind": kind,
			"content_hash":  hash,
			"size":          len(data),
			"title":         name,
		},
	}
	if repoPrefix != "" {
		node.WorkspaceID = repoPrefix
	}
	g.AddNode(node)

	for _, symID := range refs {
		g.AddEdge(&graph.Edge{
			From:     nodeID,
			To:       symID,
			Kind:     graph.EdgeReferences,
			FilePath: graphPath,
			Origin:   graph.OriginTextMatched,
		})
	}

	return Artifact{
		ID:          nodeID,
		Path:        graphPath,
		Name:        name,
		Kind:        kind,
		ContentHash: hash,
		Size:        len(data),
		References:  refs,
	}, true
}

// buildSymbolIndex maps every sufficiently-long symbol name to the
// node IDs that declare it, scoped to repoPrefix.
func buildSymbolIndex(g graph.Store, repoPrefix string) map[string][]string {
	index := make(map[string][]string)
	for _, n := range g.AllNodes() {
		switch n.Kind {
		case graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface:
		default:
			continue
		}
		if repoPrefix != "" && n.RepoPrefix != repoPrefix {
			continue
		}
		if len(n.Name) < minRefTokenLen {
			continue
		}
		index[n.Name] = append(index[n.Name], n.ID)
	}
	return index
}

// scanReferences tokenises artifact content and returns the IDs of
// every symbol whose name appears as a whole token, capped and sorted.
func scanReferences(data []byte, nameIndex map[string][]string) []string {
	if len(nameIndex) == 0 {
		return nil
	}
	scan := data
	if len(scan) > maxScanBytes {
		scan = scan[:maxScanBytes]
	}
	hits := make(map[string]bool)
	for _, tok := range identifierTokens(string(scan)) {
		for _, id := range nameIndex[tok] {
			hits[id] = true
		}
	}
	if len(hits) == 0 {
		return nil
	}
	out := make([]string, 0, len(hits))
	for id := range hits {
		out = append(out, id)
	}
	sort.Strings(out)
	if len(out) > maxRefsPerArtifact {
		out = out[:maxRefsPerArtifact]
	}
	return out
}

// identifierTokens splits text into the set of identifier-shaped
// tokens ([A-Za-z_][A-Za-z0-9_]*).
func identifierTokens(text string) []string {
	seen := make(map[string]bool)
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		tok := b.String()
		b.Reset()
		if len(tok) >= minRefTokenLen && !seen[tok] {
			seen[tok] = true
			out = append(out, tok)
		}
	}
	for _, r := range text {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// detectKind classifies an artifact file from its path when the
// manifest entry leaves Kind unset.
func detectKind(rel string) string {
	lower := strings.ToLower(rel)
	base := strings.ToLower(path.Base(rel))
	ext := path.Ext(base)
	switch ext {
	case ".sql", ".prisma":
		return "schema"
	case ".graphql", ".gql", ".proto":
		return "api"
	case ".tf", ".hcl", ".tfvars":
		return "infra"
	case ".md", ".markdown", ".rst", ".txt":
		return "doc"
	}
	if base == "kustomization.yaml" || base == "kustomization.yml" || base == "chart.yaml" {
		return "infra"
	}
	if strings.Contains(lower, "openapi") || strings.Contains(lower, "swagger") {
		return "api"
	}
	if strings.Contains(lower, "/adr") || strings.Contains(lower, "adr/") || strings.Contains(lower, "decision") {
		return "doc"
	}
	if ext == ".yaml" || ext == ".yml" || ext == ".json" {
		return "infra"
	}
	return "doc"
}

// expandGlob resolves a manifest path entry against root and returns
// the matching repo-relative file paths. A literal path, a single-
// segment glob (filepath.Glob), and a ** recursive glob are all
// supported.
func expandGlob(root, pattern string) []string {
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	if pattern == "" {
		return nil
	}
	if !strings.ContainsAny(pattern, "*?[") {
		if info, err := os.Stat(filepath.Join(root, pattern)); err == nil && !info.IsDir() {
			return []string{pattern}
		}
		return nil
	}
	if !strings.Contains(pattern, "**") {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil {
			return nil
		}
		var out []string
		for _, m := range matches {
			if info, err := os.Stat(m); err == nil && !info.IsDir() {
				if rel, err := filepath.Rel(root, m); err == nil {
					out = append(out, filepath.ToSlash(rel))
				}
			}
		}
		return out
	}
	// Recursive ** — walk the tree and segment-match each file.
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if matchGlob(pattern, rel) {
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// matchGlob reports whether path matches a glob pattern; "**" matches
// any number of path segments, "*"/"?" match within one segment.
func matchGlob(pattern, p string) bool {
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

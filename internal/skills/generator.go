// Package skills generates per-community SKILL.md files from graph analysis.
package skills

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// Generator produces per-community skill files from graph analysis results.
type Generator struct {
	communities *analysis.CommunityResult
	processes   *analysis.ProcessResult
	graph       graph.Store
	minSize     int
	maxSkills   int
}

// GeneratedSkill holds the generated SKILL.md content for one community.
type GeneratedSkill struct {
	CommunityID string
	Label       string // kebab-case, e.g. "mcp-server"
	DirName     string // e.g. "gortex-mcp-server"
	Content     string // full SKILL.md content
}

// New creates a skill generator.
func New(communities *analysis.CommunityResult, processes *analysis.ProcessResult, g graph.Store) *Generator {
	return &Generator{
		communities: communities,
		processes:   processes,
		graph:       g,
		minSize:     3,
		maxSkills:   20,
	}
}

// SetMinSize sets the minimum community size for skill generation.
func (g *Generator) SetMinSize(n int) { g.minSize = n }

// SetMaxSkills sets the maximum number of skills to generate.
func (g *Generator) SetMaxSkills(n int) { g.maxSkills = n }

// GenerateAll produces SKILL.md content for significant communities.
func (g *Generator) GenerateAll() []GeneratedSkill {
	if g.communities == nil || len(g.communities.Communities) == 0 {
		return nil
	}

	// Filter and sort communities by size descending.
	var candidates []analysis.Community
	for _, c := range g.communities.Communities {
		if c.Size >= g.minSize {
			candidates = append(candidates, c)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Size > candidates[j].Size
	})
	if len(candidates) > g.maxSkills {
		candidates = candidates[:g.maxSkills]
	}

	// Build cross-community connection map.
	crossComm := g.buildCrossCommMap()

	var skills []GeneratedSkill
	for _, c := range candidates {
		label := toKebab(c.Label)
		if label == "" {
			label = c.ID
		}

		skill := GeneratedSkill{
			CommunityID: c.ID,
			Label:       label,
			DirName:     "gortex-" + label,
			Content:     g.renderSkill(c, crossComm),
		}
		skills = append(skills, skill)
	}

	return skills
}

// GenerateRouting produces the CLAUDE.md routing table between markers.
func (g *Generator) GenerateRouting(skills []GeneratedSkill) string {
	var sb strings.Builder
	sb.WriteString("<!-- gortex:skills:start -->\n")
	sb.WriteString("## Community Skills\n\n")
	sb.WriteString("| Area | Description | Skill |\n")
	sb.WriteString("|------|-------------|-------|\n")
	for _, s := range skills {
		title := capitalizeWords(strings.ReplaceAll(s.Label, "-", " "))
		fmt.Fprintf(&sb, "| %s | %d symbols | `/gortex-%s` |\n", title, g.CommunitySize(s.CommunityID), s.Label)
	}
	sb.WriteString("<!-- gortex:skills:end -->\n")
	return sb.String()
}

// CommunitySize returns the member count for a community by ID.
func (g *Generator) CommunitySize(id string) int {
	for _, c := range g.communities.Communities {
		if c.ID == id {
			return c.Size
		}
	}
	return 0
}

func (g *Generator) renderSkill(c analysis.Community, crossComm map[string]map[string]int) string {
	var sb strings.Builder
	label := c.Label
	if label == "" {
		label = c.ID
	}

	// Frontmatter.
	kebab := toKebab(label)
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "name: gortex-%s\n", kebab)
	fmt.Fprintf(&sb, "description: \"Work in the %s area — %d symbols across %d files (%.0f%% cohesion)\"\n",
		label, c.Size, len(c.Files), c.Cohesion*100)
	sb.WriteString("---\n\n")

	// Title.
	fmt.Fprintf(&sb, "# %s\n\n", label)
	fmt.Fprintf(&sb, "%d symbols | %d files | %.0f%% cohesion\n\n", c.Size, len(c.Files), c.Cohesion*100)

	// When to use.
	sb.WriteString("## When to Use\n\n")
	sb.WriteString("Use this skill when working on files in:\n")
	for _, f := range c.Files {
		fmt.Fprintf(&sb, "- `%s`\n", f)
	}
	sb.WriteString("\n")

	// Key files table.
	fileSymbols := g.buildFileSymbolMap(c)
	if len(fileSymbols) > 0 {
		sb.WriteString("## Key Files\n\n")
		sb.WriteString("| File | Symbols |\n")
		sb.WriteString("|------|---------|\n")

		// Sort files for deterministic output.
		files := make([]string, 0, len(fileSymbols))
		for f := range fileSymbols {
			files = append(files, f)
		}
		sort.Strings(files)

		for _, f := range files {
			names := fileSymbols[f]
			if len(names) > 5 {
				names = append(names[:5], "...")
			}
			fmt.Fprintf(&sb, "| `%s` | %s |\n", f, strings.Join(names, ", "))
		}
		sb.WriteString("\n")
	}

	// Entry points.
	entryPoints := g.findEntryPoints(c)
	if len(entryPoints) > 0 {
		sb.WriteString("## Entry Points\n\n")
		for _, ep := range entryPoints {
			fmt.Fprintf(&sb, "- `%s`\n", ep)
		}
		sb.WriteString("\n")
	}

	// Connected communities.
	if connections, ok := crossComm[c.ID]; ok && len(connections) > 0 {
		sb.WriteString("## Connected Communities\n\n")
		type conn struct {
			label string
			count int
		}
		var conns []conn
		for cid, count := range connections {
			conns = append(conns, conn{label: g.communityLabel(cid), count: count})
		}
		sort.Slice(conns, func(i, j int) bool { return conns[i].count > conns[j].count })
		for _, cn := range conns {
			fmt.Fprintf(&sb, "- **%s** (%d cross-edges)\n", cn.label, cn.count)
		}
		sb.WriteString("\n")
	}

	// How to explore.
	sb.WriteString("## How to Explore\n\n")
	sb.WriteString("```\n")
	fmt.Fprintf(&sb, "get_communities with id: \"%s\"\n", c.ID)
	fmt.Fprintf(&sb, "smart_context with task: \"understand %s\", format: \"gcx\"\n", label)
	if len(entryPoints) > 0 {
		fmt.Fprintf(&sb, "find_usages with id: \"%s\", format: \"gcx\"\n", entryPoints[0])
	}
	sb.WriteString("```\n\n")
	sb.WriteString("_`format: \"gcx\"` returns the [GCX1 compact wire format](../../docs/wire-format.md) — round-trippable, ~27% fewer tokens than JSON. Drop it for JSON output; agents using `@gortex/wire` or the Go `github.com/gortexhq/gcx-go` package decode either._\n")

	return sb.String()
}

func (g *Generator) buildFileSymbolMap(c analysis.Community) map[string][]string {
	fileSymbols := make(map[string][]string)
	for _, memberID := range c.Members {
		node := g.graph.GetNode(memberID)
		if node == nil || node.Kind == graph.KindFile || node.Kind == graph.KindImport {
			continue
		}
		fileSymbols[node.FilePath] = append(fileSymbols[node.FilePath], node.Name)
	}
	return fileSymbols
}

func (g *Generator) findEntryPoints(c analysis.Community) []string {
	if g.processes == nil {
		return nil
	}
	memberSet := make(map[string]bool, len(c.Members))
	for _, m := range c.Members {
		memberSet[m] = true
	}

	var eps []string
	seen := make(map[string]bool)
	for _, p := range g.processes.Processes {
		if memberSet[p.EntryPoint] && !seen[p.EntryPoint] {
			seen[p.EntryPoint] = true
			eps = append(eps, p.EntryPoint)
		}
	}
	if len(eps) > 5 {
		eps = eps[:5]
	}
	return eps
}

func (g *Generator) buildCrossCommMap() map[string]map[string]int {
	if g.communities == nil {
		return nil
	}
	crossComm := make(map[string]map[string]int)

	for _, e := range g.graph.AllEdges() {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		fromComm := g.communities.NodeToComm[e.From]
		toComm := g.communities.NodeToComm[e.To]
		if fromComm == "" || toComm == "" || fromComm == toComm {
			continue
		}
		if crossComm[fromComm] == nil {
			crossComm[fromComm] = make(map[string]int)
		}
		crossComm[fromComm][toComm]++
	}
	return crossComm
}

func (g *Generator) communityLabel(id string) string {
	for _, c := range g.communities.Communities {
		if c.ID == id {
			if c.Label != "" {
				return c.Label
			}
			return id
		}
	}
	return id
}

var nonAlphaNum = regexp.MustCompile(`[^a-z0-9]+`)

func capitalizeWords(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

func toKebab(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, string(filepath.Separator), "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = nonAlphaNum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

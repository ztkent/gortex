package hermes

import (
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	yaml "gopkg.in/yaml.v3"
)

// gortexServerName is the key the gortex stanza lives under in the
// Hermes `mcp_servers` map. Stable across releases so re-installs
// upsert in place rather than duplicating.
const gortexServerName = "gortex"

// connectTimeoutSecs / requestTimeoutSecs match a real-world working
// Hermes ↔ gortex setup: the daemon-backed MCP server can take a
// moment to hand off on first connect and graph-heavy tools (smart_
// context, analyze) occasionally run longer than Hermes' tight
// defaults, so we give both a generous ceiling out of the box.
const (
	connectTimeoutSecs = 60
	requestTimeoutSecs = 120
)

// gortexMCPEntry builds the stdio MCP stanza Hermes expects under
// `mcp_servers.gortex`. It mirrors the shape Hermes uses for every
// other stdio server (command + args + the two timeout knobs):
//
//	gortex:
//	  command: /abs/path/to/gortex
//	  args: [mcp]
//	  connect_timeout: 60
//	  timeout: 120
//
// `gortex mcp` (no flags) connects to a running daemon and resolves
// the active workspace per MCP session, so one global stanza serves
// every repo Hermes is pointed at — no cwd-relative state to trip on.
func gortexMCPEntry(command string) *yaml.Node {
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	agents.YAMLSetMapValue(entry, "command", agents.YAMLScalar(command))

	// Flow style — `args: [mcp]` — to match the canonical Hermes
	// example and keep the inserted block as compact as the rest of
	// a hand-written config.
	args := &yaml.Node{
		Kind:    yaml.SequenceNode,
		Tag:     "!!seq",
		Style:   yaml.FlowStyle,
		Content: []*yaml.Node{agents.YAMLScalar("mcp")},
	}
	agents.YAMLSetMapValue(entry, "args", args)
	agents.YAMLSetMapValue(entry, "connect_timeout", yamlInt(connectTimeoutSecs))
	agents.YAMLSetMapValue(entry, "timeout", yamlInt(requestTimeoutSecs))
	return entry
}

// yamlInt builds an integer scalar node. Kept here rather than in the
// agents package because the generic YAMLScalar helper only covers
// strings and Hermes is the only adapter that needs typed YAML
// scalars today.
func yamlInt(n int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(n)}
}

// SkillName is the directory under ~/.hermes/skills/ that holds the
// gortex skill. Hermes discovers SKILL.md files recursively under the
// skills root, so a single `gortex/SKILL.md` is picked up regardless
// of the per-version layout.
const SkillName = "gortex"

// masterSkillCategory is the Hermes skills category the master gortex
// guide is filed under (skills/<category>/gortex/SKILL.md). Shared with
// skillCategory so the on-disk folder and the frontmatter agree.
const masterSkillCategory = "code-intelligence"

// masterSkillRaw is the static body of the user-level Hermes master
// skill: it teaches the agent to prefer gortex graph tools over raw file
// reads / text search, mirrors the Claude Code / Antigravity user-level
// instruction surface, and documents multi-repo scoping. It follows the
// Hermes SKILL.md frontmatter schema (name / description / version /
// metadata.hermes) and the documented section order. SkillBody() wraps
// it with the dynamic frontmatter fields (platforms, related_skills) and
// the slash-command index, both derived from RoutingSkillNames() so they
// never drift from the installed routing set.
const masterSkillRaw = `---
name: gortex
description: "Use for any task on a codebase indexed by the gortex daemon — searching symbols, finding usages/callers, reading code, tracing impact, refactoring, and multi-repo navigation. Prefer these graph tools over raw file reads or text search."
version: 1.0.0
metadata:
  hermes:
    tags: [code-intelligence, code-search, navigation, refactoring, mcp]
    category: code-intelligence
---

# Gortex Code Intelligence

Gortex indexes repositories into an in-memory knowledge graph and serves it over MCP. On any indexed codebase its graph tools are faster, cheaper, and more accurate than reading whole files or grepping — they return exactly the symbol, caller set, or blast radius you asked for, with zero false positives.

## When to Use

- Searching for a symbol, function, type, or where something is referenced.
- Reading a single function/method without pulling its whole file.
- Understanding architecture, tracing call chains, or checking what a change breaks.
- Refactoring: renames, extractions, and multi-file edits that must stay consistent.
- Working across more than one repository from a single session.

## Prerequisites

- The ` + "`gortex`" + ` MCP server is registered in ` + "`~/.hermes/config.yaml`" + ` under ` + "`mcp_servers.gortex`" + ` (gortex's installer wires this for you).
- The gortex daemon is running and tracking the repo: check with ` + "`gortex daemon status`" + ` in a terminal, start it with ` + "`gortex daemon start --detach`" + `, and track a repo with ` + "`gortex init`" + ` (or ` + "`gortex track <path>`" + `).
- Confirm the graph is live at the start of a task by calling the ` + "`graph_stats`" + ` tool. If ` + "`total_nodes`" + ` is 0, call ` + "`index_repository`" + ` with ` + "`path: \".\"`" + ` first.

## How to Run

Call the gortex MCP tools directly. Translate the instinct to read or grep into the matching graph query:

### Search and navigation

| Instead of...                            | Use the gortex tool...                       |
|------------------------------------------|----------------------------------------------|
| Grepping for a symbol                    | ` + "`search_symbols`" + ` (BM25 + camelCase-aware)         |
| Grepping for references                  | ` + "`find_usages`" + ` (zero false positives)             |
| Hunting for callers                      | ` + "`get_callers`" + ` / ` + "`get_call_chain`" + `                     |
| Globbing source files (` + "`**/*.go`" + `)         | ` + "`get_repo_outline`" + ` / ` + "`search_symbols`" + `                |
| Many file reads to orient on a task      | ` + "`smart_context`" + ` (one call assembles the working set) |
| Literal / regex text the symbol index misses | ` + "`search_text`" + ` (trigram-accelerated grep)         |

### Reading source

| Instead of...                            | Use the gortex tool...                       |
|------------------------------------------|----------------------------------------------|
| Reading a whole file for one function    | ` + "`get_symbol_source`" + ` (≈80% fewer tokens)          |
| Reading a file to understand it          | ` + "`get_file_summary`" + ` / ` + "`get_editing_context`" + `           |
| Reading a file to check a signature      | ` + "`get_symbol`" + ` (signature in ` + "`meta.signature`" + `)         |
| Reading a non-indexed / raw file         | ` + "`read_file`" + ` (atomic, overlay-aware)              |

### Editing and refactoring

| Instead of...                            | Use the gortex tool...                       |
|------------------------------------------|----------------------------------------------|
| A whole-file string-match edit           | ` + "`edit_file`" + ` (no pre-read; atomic; auto-reindex)  |
| A read→edit roundtrip for one symbol     | ` + "`edit_symbol`" + ` (edit by ID)                       |
| Manual find-and-replace for a rename     | ` + "`rename_symbol`" + ` (updates cross-file references)  |
| Sequencing multi-file edits by hand      | ` + "`batch_edit`" + ` (dependency-ordered, atomic)        |
| Guessing what a change breaks            | ` + "`verify_change`" + ` / ` + "`get_dependents`" + ` (blast radius)    |

### Analysis

` + "`analyze`" + ` is a unified dispatcher — pass ` + "`kind`" + ` for one of ` + "`dead_code`" + `, ` + "`hotspots`" + `, ` + "`cycles`" + `, ` + "`coverage_gaps`" + `, ` + "`todos`" + `, ` + "`sast`" + `, ` + "`impact`" + `, ` + "`cross_repo`" + `, and ~50 more. ` + "`get_architecture`" + ` gives a one-call architectural snapshot.

## Multi-repo scoping

The daemon can track several repositories at once. Scope your queries so results come from the right project:

- Call ` + "`get_active_project`" + ` to see the current scope and ` + "`set_active_project`" + ` to switch the session default.
- Most list/search tools accept a ` + "`repo`" + ` or ` + "`project`" + ` argument to target one repository for a single call without changing the session default.
- ` + "`list_repos`" + ` enumerates everything the daemon tracks; ` + "`track_repository`" + ` adds a new one.
- ` + "`analyze kind: \"cross_repo\"`" + ` and a ` + "`find_usages`" + ` partitioned by repo answer "who consumes this across all our services?".

## Quick Reference

1. ` + "`graph_stats`" + ` — confirm the daemon is up and oriented.
2. ` + "`smart_context`" + ` with the task description — assemble the minimal working set.
3. ` + "`search_symbols`" + ` / ` + "`find_usages`" + ` / ` + "`get_symbol_source`" + ` — navigate and read.
4. ` + "`get_editing_context`" + ` then ` + "`edit_symbol`" + ` / ` + "`edit_file`" + ` / ` + "`rename_symbol`" + ` / ` + "`batch_edit`" + ` — edit safely.
5. ` + "`verify_change`" + ` / ` + "`get_test_targets`" + ` — check the blast radius before and after.

## Token economy

For list-shaped responses (` + "`search_symbols`" + `, ` + "`find_usages`" + `, ` + "`analyze`" + `, ` + "`get_callers`" + `, ` + "`get_editing_context`" + `, ` + "`smart_context`" + `, …) pass ` + "`format: \"gcx\"`" + ` for the GCX1 compact wire format — round-trippable, ~27% fewer tokens. For reading source, pass ` + "`compress_bodies: true`" + ` to ` + "`read_file`" + ` / ` + "`get_symbol_source`" + ` / ` + "`get_editing_context`" + ` to elide function bodies to signatures (~30–40% of original tokens).

## Pitfalls

- Don't fall back to raw file reads / shell grep on an indexed repo "just to be quick" — the graph tools are both faster and more precise, and they keep your context budget intact.
- An empty result from ` + "`search_symbols`" + ` usually means the daemon hasn't finished warming or isn't tracking this repo — check ` + "`graph_stats`" + ` / ` + "`index_health`" + ` rather than assuming the symbol is absent.
- In a multi-repo session, an unexpected result set is often a scoping issue — verify ` + "`get_active_project`" + ` or pass an explicit ` + "`repo`" + ` argument.

## Verification

After edits, call ` + "`verify_change`" + ` (broken callers + interface implementors, cross-repo) and ` + "`get_test_targets`" + ` (the tests that cover what you touched) before declaring the task done.
`

// SkillBody renders the master gortex skill: the static guide
// (masterSkillRaw) with the dynamic frontmatter fields (platforms,
// related_skills) injected and the slash-command index appended. The
// dynamic parts derive from RoutingSkillNames() so they stay in sync
// with the routing skills actually installed.
func SkillBody() string {
	const fmClose = "    category: " + masterSkillCategory + "\n---\n"
	inject := "    category: " + masterSkillCategory + "\n" +
		"    platforms: [linux, macos, windows]\n" +
		"    related_skills: [" + strings.Join(RoutingSkillNames(), ", ") + "]\n---\n"
	return strings.Replace(masterSkillRaw, fmClose, inject, 1) + masterSkillCommands()
}

// masterSkillCommands renders a discoverability section listing the
// /gortex-* slash commands that `gortex install` registers (Hermes turns
// every installed skill into a /<name> command). Derived from
// RoutingSkillNames() so the list can't drift from what is installed.
func masterSkillCommands() string {
	names := RoutingSkillNames()
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Task playbooks (slash commands)\n\n")
	b.WriteString("`gortex install` also registers these per-task playbooks as Hermes slash commands. Reach for the one that matches your task:\n\n")
	for _, n := range names {
		b.WriteString("- `/" + n + "`\n")
	}
	return b.String()
}

// RoutingSkills returns the per-task routing skills, keyed by the
// directory under ~/.hermes/skills/. They mirror Claude Code's curated
// user-level skill set so a Hermes user gets the same task-routing
// surface — explore / impact / debug / refactor / rename / safe-edit /
// add-test / pr-review / … — that `gortex install` gives Claude Code.
//
// The bodies are the single source of truth in
// internal/agents/claudecode (reused verbatim so the two agents never
// drift) re-wrapped with Hermes frontmatter. Hermes also turns every
// installed skill into a `/skill-name` slash command, so the
// `/gortex-explore`-style cross-references in the bodies resolve to the
// sibling skills installed here.
//
// gortex-guide is excluded: the native master `gortex` skill
// (SkillBody) already fills the guide role, and shipping both would be
// a redundant entry in Hermes' skill picker.
func RoutingSkills() map[string]string {
	out := make(map[string]string, len(claudecode.GlobalSkills))
	for name, claudeBody := range claudecode.GlobalSkills {
		if name == "gortex-guide" {
			continue
		}
		out[name] = hermesSkillFromClaude(name, claudeBody)
	}
	return out
}

// RoutingSkillNames returns the routing skill directory names, sorted,
// for a stable Plan / install report and deterministic tests.
func RoutingSkillNames() []string {
	skills := RoutingSkills()
	names := make([]string, 0, len(skills))
	for name := range skills {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// hermesSkillFromClaude re-frames one Claude Code skill as a Hermes
// skill: it keeps the body verbatim and swaps the Claude frontmatter
// (name + description) for Hermes frontmatter (name + description +
// version + metadata.hermes.{tags,category}).
func hermesSkillFromClaude(name, claudeContent string) string {
	desc := claudeFrontmatterField(claudeContent, "description")
	body := stripClaudeFrontmatter(claudeContent)
	tags, category := routingSkillTaxonomy(name)

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + name + "\n")
	if desc != "" {
		b.WriteString("description: " + desc + "\n")
	}
	b.WriteString("version: 1.0.0\n")
	b.WriteString("metadata:\n")
	b.WriteString("  hermes:\n")
	b.WriteString("    tags: [" + strings.Join(tags, ", ") + "]\n")
	b.WriteString("    category: " + category + "\n")
	b.WriteString("    platforms: [linux, macos, windows]\n")
	b.WriteString("    related_skills: [" + SkillName + "]\n")
	b.WriteString("---\n")
	b.WriteString(body)
	return b.String()
}

// stripClaudeFrontmatter drops the leading `---`-fenced YAML block from
// a Claude skill body, returning just the markdown. The skill bodies
// always start with `---\n`; the first `\n---\n` after it closes the
// frontmatter.
func stripClaudeFrontmatter(s string) string {
	const open = "---\n"
	if !strings.HasPrefix(s, open) {
		return s
	}
	rest := s[len(open):]
	if _, after, found := strings.Cut(rest, "\n---\n"); found {
		return after
	}
	return s
}

// claudeFrontmatterField extracts a single-line scalar field from the
// leading frontmatter block, value verbatim (quotes and inner escapes
// preserved — the Claude descriptions are already valid YAML double-
// quoted scalars). Returns "" when the field is absent.
func claudeFrontmatterField(s, field string) string {
	const open = "---\n"
	if !strings.HasPrefix(s, open) {
		return ""
	}
	block := s[len(open):]
	if before, _, found := strings.Cut(block, "\n---\n"); found {
		block = before
	}
	prefix := field + ":"
	for line := range strings.SplitSeq(block, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// routingSkillTaxonomy assigns Hermes discovery tags + a category to a
// routing skill from its name. The per-skill topic tag (the name minus
// the `gortex-` prefix) plus a broad category make the skill findable
// in Hermes' skills_list without hand-maintaining a table per skill.
func routingSkillTaxonomy(name string) (tags []string, category string) {
	topic := strings.TrimPrefix(name, "gortex-")
	category = "code-intelligence"
	switch topic {
	case "explore", "onboarding", "cross-repo-usage", "dataflow-trace":
		category = "navigation"
	case "impact", "co-change", "architecture-review", "quality-audit", "pr-review", "pr-review-agent", "episode-replay":
		category = "analysis"
	case "debug", "incident-investigation":
		category = "debugging"
	case "refactor", "rename", "extract-function", "safe-edit", "fix-all":
		category = "refactoring"
	case "add-test":
		category = "testing"
	}
	return []string{"code-intelligence", "mcp", topic}, category
}

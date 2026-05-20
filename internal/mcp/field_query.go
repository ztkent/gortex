package mcp

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// fieldQuery is a search query split into its free-text component and
// any field-qualified clauses (kind: / lang: / path: / repo: /
// project:) lifted out of the raw query string.
type fieldQuery struct {
	Text    string // residual free text after clauses are removed
	Kind    string // kind: clause — comma-separated node kinds
	Lang    string // lang: / language: clause — node language
	Path    string // path: clause — file-path substring
	Repo    string // repo: clause — repository prefix
	Project string // project: clause — project slug
}

// hasFieldFilters reports whether any post-filter clause (kind / lang
// / path / repo) was supplied. project: is excluded — it merges into
// the query scope rather than acting as a post-filter.
func (fq fieldQuery) hasFieldFilters() bool {
	return fq.Kind != "" || fq.Lang != "" || fq.Path != "" || fq.Repo != ""
}

// parseFieldQuery splits a raw search string into its free text and
// field-qualified clauses. A whitespace-delimited token of the form
// `field:value` is lifted into a clause when `field` is one of the
// recognised names (kind, lang/language, path, repo, project) and the
// value is non-empty; every other token — including identifiers that
// merely contain a colon, such as `pkg::Type` or a URL — stays in the
// free text verbatim. Field names are case-insensitive; a field that
// appears more than once keeps the last value.
func parseFieldQuery(raw string) fieldQuery {
	var fq fieldQuery
	var text []string
	for _, tok := range strings.Fields(raw) {
		name, value, ok := strings.Cut(tok, ":")
		if !ok || value == "" {
			text = append(text, tok)
			continue
		}
		switch strings.ToLower(name) {
		case "kind":
			fq.Kind = value
		case "lang", "language":
			fq.Lang = value
		case "path":
			fq.Path = value
		case "repo":
			fq.Repo = value
		case "project":
			fq.Project = value
		default:
			text = append(text, tok)
		}
	}
	fq.Text = strings.Join(text, " ")
	return fq
}

// normalizeLang folds the common short language aliases (ts, js, py,
// …) onto the canonical language names the indexer stamps on nodes.
// An unrecognised value is returned lowercased and trimmed.
func normalizeLang(l string) string {
	switch v := strings.ToLower(strings.TrimSpace(l)); v {
	case "ts":
		return "typescript"
	case "js":
		return "javascript"
	case "py":
		return "python"
	case "rb":
		return "ruby"
	case "rs":
		return "rust"
	case "kt":
		return "kotlin"
	case "yml":
		return "yaml"
	default:
		return v
	}
}

// applyFieldFilters narrows a node slice by the lang / path / repo
// clauses of a field query. The kind clause is applied separately via
// filterNodesByKind so it can merge with the explicit kind argument.
// Language matching is exact (after alias folding); path matches as a
// case-insensitive substring of the node's file path; repo matches
// the node's repository prefix exactly. Empty clauses are skipped, and
// a node with no repo prefix (single-repo mode) is never dropped by a
// repo clause — mirroring filterNodes.
func applyFieldFilters(nodes []*graph.Node, fq fieldQuery) []*graph.Node {
	lang := normalizeLang(fq.Lang)
	path := strings.ToLower(strings.TrimSpace(fq.Path))
	repo := strings.TrimSpace(fq.Repo)
	if lang == "" && path == "" && repo == "" {
		return nodes
	}
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if lang != "" && strings.ToLower(n.Language) != lang {
			continue
		}
		if path != "" && !strings.Contains(strings.ToLower(n.FilePath), path) {
			continue
		}
		if repo != "" && n.RepoPrefix != "" && n.RepoPrefix != repo {
			continue
		}
		out = append(out, n)
	}
	return out
}

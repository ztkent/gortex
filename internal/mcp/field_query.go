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

// applyPathFilter narrows a node slice to those whose file path sits
// under one of the given sub-paths. Unlike the inline `path:` clause
// (a loose substring match via applyFieldFilters), this is an
// ANCHORED, slash-segment-normalised prefix test: the path
// "services/billing" matches "services/billing/invoice.go" but NOT
// "other/services/billingX/y.go" -- the prefix must align on a
// directory boundary at the start of the path.
//
// In multi-repo mode a node's FilePath is repo-prefixed
// ("<repo>/services/billing/x.go"); the repo prefix is stripped
// before matching so a sub-path is expressed relative to the repo
// root regardless of repo mode.
//
// An empty paths slice is a no-op (every node passes). A node passes
// when it matches ANY of the paths.
func applyPathFilter(nodes []*graph.Node, paths []string) []*graph.Node {
	norm := normalizePathPrefixes(paths)
	if len(norm) == 0 {
		return nodes
	}
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if pathMatchesAnyPrefix(repoRelativePath(n), norm) {
			out = append(out, n)
		}
	}
	return out
}

// repoRelativePath returns a node's file path with its repo prefix
// stripped and back-slashes normalised to forward slashes, so a
// sub-path filter is always expressed relative to the repo root.
func repoRelativePath(n *graph.Node) string {
	p := strings.ReplaceAll(n.FilePath, "\\", "/")
	if n.RepoPrefix != "" {
		p = strings.TrimPrefix(p, n.RepoPrefix+"/")
	}
	return p
}

// normalizePathPrefixes cleans a set of sub-path filters: trims
// whitespace, normalises separators, strips a leading "./" and any
// leading/trailing slashes, and drops empties and duplicates.
func normalizePathPrefixes(paths []string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, p := range paths {
		p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
		p = strings.TrimPrefix(p, "./")
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// pathMatchesAnyPrefix reports whether a repo-relative file path sits
// under any of the (already-normalised) sub-path prefixes. A prefix
// matches when the path equals it exactly or continues past it at a
// slash boundary -- so "services/billing" matches
// "services/billing/x.go" but not "services/billingX/x.go".
func pathMatchesAnyPrefix(path string, prefixes []string) bool {
	for _, pre := range prefixes {
		if path == pre {
			return true
		}
		if strings.HasPrefix(path, pre+"/") {
			return true
		}
	}
	return false
}

package mcp

import (
	"github.com/pmezard/go-difflib/difflib"
)

// unifiedDiff renders a standard unified diff (3 lines of context) between
// oldContent and newContent, labelled with the given repo-relative path. It
// powers the dry_run previews of the edit tools so an agent can see exactly
// what a write would change — and review it — before committing the edit.
// Returns "" when the inputs are identical or a diff cannot be produced.
func unifiedDiff(path, oldContent, newContent string) string {
	if oldContent == newContent {
		return ""
	}
	out, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(oldContent),
		B:        difflib.SplitLines(newContent),
		FromFile: "a/" + path,
		ToFile:   "b/" + path,
		Context:  3,
	})
	if err != nil {
		return ""
	}
	return out
}

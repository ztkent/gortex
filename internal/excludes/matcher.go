package excludes

import (
	"path/filepath"
	"strings"

	ignore "github.com/sabhiram/go-gitignore"
)

// Matcher tests whether a path should be excluded from indexing/watching.
// It is safe for concurrent reads after construction.
type Matcher struct {
	ign      *ignore.GitIgnore
	patterns []string
}

// New compiles the given patterns into a Matcher. A nil/empty list is
// valid and will match nothing.
func New(patterns []string) *Matcher {
	cleaned := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		cleaned = append(cleaned, p)
	}
	return &Matcher{
		ign:      ignore.CompileIgnoreLines(cleaned...),
		patterns: cleaned,
	}
}

// Patterns returns the cleaned pattern list (empties and comments removed).
func (m *Matcher) Patterns() []string {
	if m == nil {
		return nil
	}
	out := make([]string, len(m.patterns))
	copy(out, m.patterns)
	return out
}

// MatchRel reports whether a repo-root-relative path is excluded.
// Path separators are normalised to forward slashes before matching.
func (m *Matcher) MatchRel(relPath string) bool {
	if m == nil || m.ign == nil {
		return false
	}
	rel := filepath.ToSlash(relPath)
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" || rel == "." {
		return false
	}
	return m.ign.MatchesPath(rel)
}

// MatchAbs reports whether an absolute path under root is excluded.
// Returns false if path is not under root.
func (m *Matcher) MatchAbs(absPath, root string) bool {
	if m == nil || m.ign == nil {
		return false
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return false
	}
	return m.MatchRel(rel)
}

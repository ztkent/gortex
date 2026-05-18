package wiki

import (
	"path/filepath"
	"regexp"
	"strings"
)

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// slugify lowercases s and collapses runs of non-alphanumerics into a
// single dash. Returns "" only for the empty input.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugRE.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// RepoSlugFromPath derives a filesystem-safe slug for the per-repo
// directory under wiki/. Uses the basename of the absolute path
// (after resolving symlinks where possible) and falls back to "repo"
// when the basename is empty or just "/".
func RepoSlugFromPath(repoPath string) string {
	if repoPath == "" {
		return "repo"
	}
	abs, err := filepath.Abs(repoPath)
	if err == nil {
		repoPath = abs
	}
	base := filepath.Base(filepath.Clean(repoPath))
	if base == "/" || base == "." || base == "" {
		return "repo"
	}
	if s := slugify(base); s != "" {
		return s
	}
	return "repo"
}

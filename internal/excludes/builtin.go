// Package excludes provides a unified path-exclusion matcher used by
// both the indexer's initial walk and the watcher's live event filter.
// Patterns follow .gitignore semantics (via go-gitignore): leading '/'
// anchors at the root, trailing '/' restricts to directories, '!' negates,
// '**' matches any number of path segments.
package excludes

// Builtin is the superset of directory/file patterns that Gortex always
// excludes, regardless of user config. It merges what the indexer and
// watcher used to maintain as two divergent hardcoded lists.
//
// Users can re-include an entry by listing "!pattern" in global,
// RepoEntry, or workspace config.
var Builtin = []string{
	".git/",
	".hg/",
	".svn/",
	".terraform/",
	".gortex-cache/",
	".claude/",
	".kiro/",
	"node_modules/",
	"vendor/",
	".venv/",
	"venv/",
	"__pycache__/",
	".mypy_cache/",
	".tox/",
	".next/",
	"target/",
	"build/",
	"dist/",
	"*.tmp",
	"*.swp",
}

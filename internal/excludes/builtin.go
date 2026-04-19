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
	// Package-manager + build dirs for non-JS/non-Go ecosystems. These
	// are indexed-by-default without these entries, which pollutes the
	// graph with upstream code (e.g. CocoaPods' sqlite3.c — 150k+ lines)
	// that users can't act on. Names are unambiguous — no first-party
	// project uses `Pods/` or `.dart_tool/` for its own source.
	"Pods/",        // CocoaPods (iOS/macOS)
	".gradle/",     // Gradle build cache (Android/JVM)
	".bundle/",     // Ruby Bundler cache
	".dart_tool/",  // Dart/Flutter build cache
	".pub-cache/",  // Dart global pub cache, occasionally vendored
	"*.tmp",
	"*.swp",
}

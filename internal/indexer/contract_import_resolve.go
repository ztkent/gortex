package indexer

import (
	"os"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/tsalias"
)

// disambiguateBareTypesViaImports is the post-pass that handles bare
// type refs UpgradeBareTypeRefs left alone because the lookup
// returned ≥2 same-repo candidates. The classic case is a TS web app
// that defines two `DashboardSnapshot` types — one in
// `web/src/lib/schema.ts` (a `type` alias) and one in
// `web/src/lib/types.ts` (an `interface`). The bare name has two
// graph nodes; only the consumer's own `import` statement decides
// which one was actually referenced.
//
// We re-read the contract's source file, parse its TS / JS imports,
// and pick the candidate whose graph FilePath matches an imported
// module. When exactly one candidate matches, the meta entry is
// rewritten to its fully-qualified ID so the downstream
// attachInlinedShapes pass can fold its field shape into the
// contract's Meta.
//
// Languages other than TS / JS are skipped — Go disambiguates
// bare-name collisions via package qualification (`pkg.Type`) and the
// in-file resolveTypeInFile pass already handles those.
func (mi *MultiIndexer) disambiguateBareTypesViaImports(cr *contracts.Registry, g *graph.Graph) {
	srcCache := map[string][]byte{}
	importCache := map[string]map[string]string{}

	for _, c := range cr.All() {
		if c.Meta == nil {
			continue
		}
		if !isImportResolvableLang(c.FilePath) {
			continue
		}
		patched := false
		items := cr.ByID(c.ID)
		for i := range items {
			if items[i].FilePath != c.FilePath || items[i].Meta == nil {
				continue
			}
			for _, key := range []string{"response_type", "request_type"} {
				name, _ := items[i].Meta[key].(string)
				if name == "" || strings.Contains(name, "::") {
					continue
				}
				resolved := mi.resolveBareTypeViaImports(c.FilePath, name, g, srcCache, importCache)
				if resolved == "" {
					continue
				}
				items[i].Meta[key] = resolved
				patched = true
			}
		}
		if patched {
			cr.ReplaceByID(c.ID, items)
		}
	}
}

// resolveBareTypeViaImports looks up `name` among the bare-type
// candidates in the merged graph and returns the unambiguous match
// reachable via an import statement in `srcFile`. Returns "" when
// the lookup is still ambiguous or no candidate matches an import
// (so the caller leaves the bare name in place).
func (mi *MultiIndexer) resolveBareTypeViaImports(
	srcFile, name string,
	g *graph.Graph,
	srcCache map[string][]byte,
	importCache map[string]map[string]string,
) string {
	candidates := g.FindNodesByName(name)
	if len(candidates) == 0 {
		return ""
	}
	var typed []*graph.Node
	for _, n := range candidates {
		if n.Kind == graph.KindType || n.Kind == graph.KindInterface {
			typed = append(typed, n)
		}
	}
	if len(typed) < 2 {
		// 0 candidates → nothing to do; 1 candidate would already have
		// been caught by UpgradeBareTypeRefs, so we don't try to redo
		// its work here.
		return ""
	}

	imports, ok := importCache[srcFile]
	if !ok {
		src := mi.cachedSource(srcFile, srcCache)
		if len(src) == 0 {
			importCache[srcFile] = nil
			return ""
		}
		imports = mi.parseImportsFor(string(src), srcFile)
		importCache[srcFile] = imports
	}
	if len(imports) == 0 {
		return ""
	}
	wantFile, ok := imports[name]
	if !ok {
		return ""
	}
	// Follow re-export chains: a symbol imported from a barrel that
	// only `export { X } from './x'`s — or a Rust module that only
	// `pub use`s X — is defined in the terminal module, not the
	// re-exporting one.
	reachable := mi.followReExportChain(wantFile, name, srcCache)
	for _, n := range typed {
		if reachable[n.FilePath] {
			return n.ID
		}
	}
	return ""
}

// tsAliasCache caches the per-repo Collection of tsconfig/jsconfig
// alias maps. Loaded lazily on first lookup for a repoPrefix and
// reused across all import resolutions in the same session. A nil
// entry means "scanned, no usable config" — distinct from "not yet
// scanned" (missing key).
var (
	tsAliasCache   = map[string]*tsalias.Collection{}
	tsAliasCacheMu sync.Mutex
)

// tsAliasMapFor returns the nearest-ancestor alias map for srcFile
// (and the repo prefix the resolved path should be prefixed with).
// srcFile is a repo-prefixed path; we determine the repo by matching
// it against tracked repos, walk that repo's filesystem root for
// config files (cached), and pick the scope nearest to srcFile.
//
// Returns (nil, "") when the repo can't be located or no usable
// config exists — callers must handle that as "no alias resolution
// available, fall through to bare-name behaviour."
func (mi *MultiIndexer) tsAliasMapFor(srcFile string) (*tsalias.Map, string) {
	if srcFile == "" {
		return nil, ""
	}
	for _, m := range mi.AllMetadata() {
		prefix := m.RepoPrefix
		if prefix == "" || !strings.HasPrefix(srcFile, prefix+"/") {
			continue
		}
		coll := loadTSAliasCollection(prefix, m.RootPath)
		if coll == nil {
			return nil, prefix
		}
		rel := strings.TrimPrefix(srcFile, prefix+"/")
		return coll.FindForFile(path.Dir(rel)), prefix
	}
	return nil, ""
}

func loadTSAliasCollection(prefix, rootPath string) *tsalias.Collection {
	tsAliasCacheMu.Lock()
	defer tsAliasCacheMu.Unlock()
	if c, ok := tsAliasCache[prefix]; ok {
		return c
	}
	c := tsalias.Load(rootPath)
	tsAliasCache[prefix] = c
	return c
}

// readFileFromAnyRepo finds the on-disk bytes for a repo-prefixed
// file path by walking tracked-repo metadata. Mirrors readNodeSource
// but takes the path directly so callers don't need a graph node.
func (mi *MultiIndexer) readFileFromAnyRepo(filePath string) ([]byte, bool) {
	if filePath == "" {
		return nil, false
	}
	for _, m := range mi.AllMetadata() {
		prefix := m.RepoPrefix
		if prefix == "" || !strings.HasPrefix(filePath, prefix+"/") {
			continue
		}
		rel := strings.TrimPrefix(filePath, prefix+"/")
		data, ok := readDiskFile(joinPath(m.RootPath, rel))
		if ok {
			return data, true
		}
	}
	return nil, false
}

// joinPath joins a root and relative path with a single separator,
// avoiding the import of "path/filepath" inside this leaf helper so
// the file's surface-area stays minimal.
func joinPath(root, rel string) string {
	if root == "" {
		return rel
	}
	if strings.HasSuffix(root, "/") {
		return root + rel
	}
	return root + "/" + rel
}

// readDiskFile is a small indirection so tests can swap in an
// in-memory fixture without touching the on-disk reader.
var readDiskFile = func(absPath string) ([]byte, bool) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, false
	}
	return data, true
}

// tsImportRe matches `import { A, B as C } from '...'`,
// `import type { A } from '...'`, `import A from '...'`, and
// `import * as A from '...'`. Capture groups:
//
//	1: named-import body (between `{` and `}`) — empty for default /
//	   namespace imports, in which case group 4 carries the bound
//	   name.
//	2: default / namespace identifier (the bare ident or `* as X`)
//	3: module path
var tsImportRe = regexp.MustCompile(
	`(?m)^\s*import\s+(?:type\s+)?(?:\{([^}]*)\}|([A-Za-z_$][\w$]*|\*\s+as\s+[A-Za-z_$][\w$]*))(?:\s*,\s*\{([^}]*)\})?\s+from\s+['"]([^'"]+)['"]`,
)

// parseTSImports walks the import lines of a TypeScript / JavaScript
// source file and returns name → absolute repo-prefixed file path.
// `srcFile` is the importing file's own repo-prefixed path; it
// anchors relative module specifiers like `'./schema'`. Bare module
// specifiers (`'react'`) are skipped — they don't resolve to a graph
// file the local repo owns. tsconfig-style path aliases (`@/lib/api`,
// `$utils/format`) ARE resolved when an alias map is provided.
func parseTSImports(src, srcFile string, aliasMap *tsalias.Map, repoPrefix string) map[string]string {
	matches := tsImportRe.FindAllStringSubmatch(src, -1)
	if len(matches) == 0 {
		return nil
	}
	out := map[string]string{}
	srcDir := path.Dir(srcFile)
	for _, m := range matches {
		named := m[1]
		defaultOrStar := m[2]
		extraNamed := m[3]
		modulePath := m[4]
		resolved := resolveTSModulePath(modulePath, srcDir, aliasMap, repoPrefix)
		if resolved == "" {
			continue
		}
		for _, name := range splitTSImportClause(named) {
			out[name] = resolved
		}
		for _, name := range splitTSImportClause(extraNamed) {
			out[name] = resolved
		}
		if defaultOrStar != "" {
			ident := defaultOrStar
			if strings.HasPrefix(ident, "*") {
				if i := strings.LastIndex(ident, " "); i >= 0 {
					ident = strings.TrimSpace(ident[i+1:])
				}
			}
			if ident != "" {
				out[ident] = resolved
			}
		}
	}
	return out
}

// splitTSImportClause unpacks a brace-delimited import list like
// `Foo, Bar as Baz, type Qux` into the local-binding names a caller
// would reference (`Foo`, `Baz`, `Qux`). The `type` keyword and
// `as <alias>` rebinds are normalised; commas inside the body are
// the only separator we care about.
func splitTSImportClause(body string) []string {
	if body == "" {
		return nil
	}
	parts := strings.Split(body, ",")
	out := make([]string, 0, len(parts))
	for _, raw := range parts {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		entry = strings.TrimPrefix(entry, "type ")
		entry = strings.TrimSpace(entry)
		if i := strings.Index(entry, " as "); i >= 0 {
			entry = strings.TrimSpace(entry[i+4:])
		}
		if entry == "" {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// resolveTSModulePath turns a TS/JS module specifier into the
// repo-prefixed file path of the imported source, or "" when the
// specifier is unresolvable (third-party module with no matching
// alias). Resolution order:
//
//  1. Relative path (`./foo`, `../bar/baz`) — anchored at srcDir.
//  2. tsconfig path alias (`@/lib/foo`, `$utils/format`) — looked up
//     against aliasMap; the resolved repo-relative target is prefixed
//     with repoPrefix so it lines up with graph FilePaths.
//
// We don't probe the disk; the caller matches the resolved path
// against a candidate's FilePath, so we just append the canonical
// `.ts` extension when none is present. `.tsx` / `.js` / `.jsx`
// paths are returned as-is when the user wrote them explicitly.
// Directory imports resolving to `index.*` are NOT handled — the
// resolver returns the bare-stem path; if the candidate type lives
// in `<dir>/index.ts` the upgrade falls through and the bare name
// is left in place (acceptable: the dashboard still renders the
// bare type chip).
func resolveTSModulePath(modulePath, srcDir string, aliasMap *tsalias.Map, repoPrefix string) string {
	if modulePath == "" {
		return ""
	}
	if strings.HasPrefix(modulePath, "./") || strings.HasPrefix(modulePath, "../") {
		joined := path.Clean(path.Join(srcDir, modulePath))
		switch path.Ext(joined) {
		case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
			return joined
		}
		return joined + ".ts"
	}
	// Bare specifier — try alias resolution. tsalias.Resolve returns a
	// repo-relative path with the extension stripped; we re-prefix it
	// with repoPrefix so it lines up with graph FilePaths, then add
	// the canonical `.ts` so the caller can match against an indexed
	// file. Returning "" still leaves the bare type name in place.
	if aliasMap == nil {
		return ""
	}
	repoRel := tsalias.Resolve(aliasMap, modulePath)
	if repoRel == "" {
		return ""
	}
	full := repoRel
	if repoPrefix != "" {
		full = repoPrefix + "/" + repoRel
	}
	switch path.Ext(full) {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
		return full
	}
	return full + ".ts"
}

// maxReExportDepth bounds barrel re-export chain following so a
// pathological circular `export * from` set can't loop forever.
const maxReExportDepth = 8

// tsReExportRe matches `export ... from '...'` re-export statements:
//
//	export * from './x'
//	export * as ns from './x'   (namespace — ignored for name-following)
//	export { A, B as C } from './x'
//	export type { T } from './x'
var tsReExportRe = regexp.MustCompile(
	`export\s+(?:type\s+)?(?:(\*(?:\s+as\s+\w+)?)|\{([^}]*)\})\s*from\s*['"]([^'"]+)['"]`)

// tsReExport is one parsed `export ... from` re-export statement.
type tsReExport struct {
	star bool
	// names maps an exported name to its name in the source module
	// (they differ under `export { Real as Public }`).
	names    map[string]string
	fromFile string
}

// tsFileCandidates expands a resolved TS/JS module path into the
// concrete files it might be on disk. resolveTSModulePath always
// guesses `.ts`, but the real file is often `.tsx`, or the specifier
// pointed at a directory whose entry point is `index.ts` (the barrel).
func tsFileCandidates(resolved string) []string {
	if resolved == "" {
		return nil
	}
	stem := resolved
	switch path.Ext(resolved) {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
		stem = strings.TrimSuffix(resolved, path.Ext(resolved))
	}
	out := []string{resolved}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".d.ts", ".mts", ".cts"} {
		out = append(out, stem+ext, stem+"/index"+ext)
	}
	seen := make(map[string]bool, len(out))
	uniq := out[:0]
	for _, c := range out {
		if !seen[c] {
			seen[c] = true
			uniq = append(uniq, c)
		}
	}
	return uniq
}

// parseTSReExports extracts barrel re-export statements from a TS/JS
// source file, resolving each `from` specifier the same way
// parseTSImports resolves an import.
func parseTSReExports(src, srcFile string, aliasMap *tsalias.Map, repoPrefix string) []tsReExport {
	matches := tsReExportRe.FindAllStringSubmatch(src, -1)
	if len(matches) == 0 {
		return nil
	}
	srcDir := path.Dir(srcFile)
	var out []tsReExport
	for _, m := range matches {
		resolved := resolveTSModulePath(m[3], srcDir, aliasMap, repoPrefix)
		if resolved == "" {
			continue
		}
		re := tsReExport{fromFile: resolved}
		if strings.TrimSpace(m[1]) == "*" {
			re.star = true
			out = append(out, re)
			continue
		}
		if m[1] != "" {
			// `export * as ns` — a namespace re-export; it does not
			// transparently forward individual names.
			continue
		}
		re.names = map[string]string{}
		for _, raw := range strings.Split(m[2], ",") {
			entry := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "type "))
			if entry == "" {
				continue
			}
			orig, exported := entry, entry
			if i := strings.Index(entry, " as "); i >= 0 {
				orig = strings.TrimSpace(entry[:i])
				exported = strings.TrimSpace(entry[i+4:])
			}
			if orig != "" && exported != "" {
				re.names[exported] = orig
			}
		}
		if len(re.names) > 0 {
			out = append(out, re)
		}
	}
	return out
}

// cachedSource reads file's bytes through the per-pass cache, using
// readFileFromAnyRepo for misses. A nil cache entry records "absent".
func (mi *MultiIndexer) cachedSource(file string, srcCache map[string][]byte) []byte {
	if src, hit := srcCache[file]; hit {
		return src
	}
	data, found := mi.readFileFromAnyRepo(file)
	if !found {
		srcCache[file] = nil
		return nil
	}
	srcCache[file] = data
	return data
}

// reExportEdge is one parsed re-export statement in a language-neutral
// shape: a `star` glob re-export (`export *` / `pub use mod::*`) or a
// `names` map of exported-name → name-in-the-source-module. fromFile is
// the resolved repo-prefixed path of the module being re-exported.
type reExportEdge struct {
	star     bool
	names    map[string]string
	fromFile string
}

// rustReExportRe matches a `pub use` (or `pub(crate) use`) re-export
// statement. A plain `use` carries no visibility modifier and is a
// private import — never a re-export — so the leading `pub` is
// mandatory. Capture groups:
//
//	1: the use path, up to but excluding a trailing `::*` glob,
//	   `::{...}` list, or `::Symbol` final segment
//	2: `*` when the statement is a glob re-export (`pub use mod::*`)
//	3: the brace body of a list re-export (`pub use mod::{A, B}`)
//	4: the final path segment of a single-symbol re-export
//	   (`pub use mod::Symbol`) — possibly `Orig as Public`
var rustReExportRe = regexp.MustCompile(
	`(?m)\bpub(?:\s*\([^)]*\))?\s+use\s+([\w:]+?)\s*::\s*(?:(\*)|\{([^}]*)\}|(\w+(?:\s+as\s+\w+)?))\s*;`)

// rustFileCandidates expands a resolved Rust module path into the
// concrete files it might be on disk. A module `foo` lives either in
// `foo.rs` or in `foo/mod.rs`; this mirrors tsFileCandidates' role of
// turning a logical module reference into matchable file paths. The
// input is the `.rs`-suffixed module path resolveRustModulePath
// produces; the suffix is stripped to recover the stem.
func rustFileCandidates(modulePath string) []string {
	if modulePath == "" {
		return nil
	}
	stem := strings.TrimSuffix(modulePath, ".rs")
	if strings.HasSuffix(stem, "/mod") {
		// Already a directory-module file path — keep it verbatim.
		return []string{stem + ".rs"}
	}
	return []string{stem + ".rs", stem + "/mod.rs"}
}

// rustCrateRoot returns the crate source root for a repo-prefixed Rust
// file — the nearest ancestor directory named `src`, or the file's own
// directory when the layout has no `src` (a flat crate). `crate::`
// paths are anchored here.
func rustCrateRoot(srcFile string) string {
	dir := path.Dir(srcFile)
	for d := dir; d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if path.Base(d) == "src" {
			return d
		}
	}
	return dir
}

// resolveRustModulePath turns a Rust module path (the part of a
// `pub use` before its final symbol / glob / list) into a repo-prefixed,
// `.rs`-suffixed module path, anchored against the re-exporting file.
// `crate::` is anchored at the crate src root; `self::` at the current
// module directory; `super::` at the parent. A leading bare segment is
// treated as a submodule of the current file's directory — the
// best-effort local candidate; genuine external-crate paths simply
// fail to match a graph file, which leaves the bare name in place. The
// `.rs` suffix lets downstream language dispatch recognise the result
// as Rust even though a logical module name carries no extension;
// rustFileCandidates strips it back to a stem.
func resolveRustModulePath(modPath, srcFile string) string {
	segs := strings.Split(modPath, "::")
	for len(segs) > 0 && segs[0] == "" {
		segs = segs[1:]
	}
	if len(segs) == 0 {
		return ""
	}
	dir := path.Dir(srcFile)
	switch segs[0] {
	case "crate":
		dir = rustCrateRoot(srcFile)
		segs = segs[1:]
	case "self":
		segs = segs[1:]
	case "super":
		for len(segs) > 0 && segs[0] == "super" {
			dir = path.Dir(dir)
			segs = segs[1:]
		}
	}
	if len(segs) == 0 {
		// `pub use crate::*` / `pub use self::*` — re-exporting the
		// module's own directory; nothing more specific to anchor.
		return path.Clean(dir) + ".rs"
	}
	return path.Clean(path.Join(dir, path.Join(segs...))) + ".rs"
}

// parseRustReExports extracts `pub use` re-export statements from a
// Rust source file, resolving each module path to a repo-prefixed
// module stem the same way parseTSReExports resolves an `export ...
// from` specifier.
func parseRustReExports(src, srcFile string) []reExportEdge {
	matches := rustReExportRe.FindAllStringSubmatch(src, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []reExportEdge
	for _, m := range matches {
		fromMod := resolveRustModulePath(m[1], srcFile)
		if fromMod == "" {
			continue
		}
		re := reExportEdge{fromFile: fromMod}
		if m[2] == "*" {
			re.star = true
			out = append(out, re)
			continue
		}
		re.names = map[string]string{}
		switch {
		case m[3] != "":
			// `pub use mod::{Orig, Other as Public}` — a list.
			for _, raw := range strings.Split(m[3], ",") {
				orig, exported := splitRustUseEntry(raw)
				if orig != "" && exported != "" {
					re.names[exported] = orig
				}
			}
		case m[4] != "":
			// `pub use mod::Symbol` / `pub use mod::Orig as Public`.
			orig, exported := splitRustUseEntry(m[4])
			if orig != "" && exported != "" {
				re.names[exported] = orig
			}
		}
		if len(re.names) > 0 {
			out = append(out, re)
		}
	}
	return out
}

// splitRustUseEntry normalises a single `use`-list entry, resolving an
// `Orig as Public` rebind to (sourceName, exportedName). A plain entry
// returns the same name twice.
func splitRustUseEntry(raw string) (orig, exported string) {
	entry := strings.TrimSpace(raw)
	if entry == "" {
		return "", ""
	}
	orig, exported = entry, entry
	if i := strings.Index(entry, " as "); i >= 0 {
		orig = strings.TrimSpace(entry[:i])
		exported = strings.TrimSpace(entry[i+4:])
	}
	return orig, exported
}

// rustImportRe matches any `use` statement (re-export or private
// import) — the consumer side, where a private `use` is a legitimate
// import to resolve. Capture groups mirror rustReExportRe groups 1-4
// minus the leading visibility modifier.
var rustImportRe = regexp.MustCompile(
	`(?m)\buse\s+([\w:]+?)\s*::\s*(?:(\*)|\{([^}]*)\}|(\w+(?:\s+as\s+\w+)?))\s*;`)

// parseRustImports walks the `use` statements of a Rust source file
// and returns local-binding-name → the repo-prefixed module stem the
// name was imported from. It is the Rust counterpart of parseTSImports:
// the caller follows a re-export chain from the returned module to
// reach the symbol's real definition. Glob imports (`use mod::*`) are
// skipped — they bind no specific name to follow.
func parseRustImports(src, srcFile string) map[string]string {
	matches := rustImportRe.FindAllStringSubmatch(src, -1)
	if len(matches) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, m := range matches {
		if m[2] == "*" {
			continue
		}
		fromMod := resolveRustModulePath(m[1], srcFile)
		if fromMod == "" {
			continue
		}
		switch {
		case m[3] != "":
			for _, raw := range strings.Split(m[3], ",") {
				if _, exported := splitRustUseEntry(raw); exported != "" {
					out[exported] = fromMod
				}
			}
		case m[4] != "":
			if _, exported := splitRustUseEntry(m[4]); exported != "" {
				out[exported] = fromMod
			}
		}
	}
	return out
}

// isRustFile reports whether a repo-prefixed path is a Rust source
// file — the re-export follower parses these with the `pub use`
// grammar rather than the TS `export ... from` grammar.
func isRustFile(filePath string) bool {
	return path.Ext(filePath) == ".rs"
}

// fileCandidatesFor expands a logical module/file reference into the
// concrete on-disk file paths it might map to, picking the TS or Rust
// expansion by file extension.
func fileCandidatesFor(file string) []string {
	if isRustFile(file) {
		return rustFileCandidates(file)
	}
	return tsFileCandidates(file)
}

// parseImportsFor maps each imported local-binding name to the
// repo-prefixed module / file it was imported from, dispatching to the
// TS (`import ... from`) or Rust (`use`) parser by file extension.
func (mi *MultiIndexer) parseImportsFor(src, srcFile string) map[string]string {
	if isRustFile(srcFile) {
		return parseRustImports(src, srcFile)
	}
	aliasMap, aliasPrefix := mi.tsAliasMapFor(srcFile)
	return parseTSImports(src, srcFile, aliasMap, aliasPrefix)
}

// reExportsFor reads the re-export statements out of one source file,
// dispatching to the TS (`export ... from`) or Rust (`pub use`) parser
// by the file's extension. Returns nil for any other language.
func (mi *MultiIndexer) reExportsFor(src, srcPath string) []reExportEdge {
	if isRustFile(srcPath) {
		return parseRustReExports(src, srcPath)
	}
	aliasMap, aliasPrefix := mi.tsAliasMapFor(srcPath)
	tsRe := parseTSReExports(src, srcPath, aliasMap, aliasPrefix)
	if len(tsRe) == 0 {
		return nil
	}
	out := make([]reExportEdge, len(tsRe))
	for i, re := range tsRe {
		out[i] = reExportEdge(re)
	}
	return out
}

// followReExportChain returns the set of concrete file paths reachable
// from startFile by following re-exports of `name` — startFile itself
// plus every module a transparent `export *` / `export { name }`
// (TypeScript) or `pub use` (Rust) chain forwards through, up to
// maxReExportDepth. A symbol's real definition is in one of the
// returned files, so a caller matching an import target against graph
// nodes resolves through the barrel / re-exporting module.
func (mi *MultiIndexer) followReExportChain(startFile, name string, srcCache map[string][]byte) map[string]bool {
	reachable := map[string]bool{}
	for _, c := range fileCandidatesFor(startFile) {
		reachable[c] = true
	}
	type step struct{ file, name string }
	seen := map[step]bool{{startFile, name}: true}
	queue := []step{{startFile, name}}
	for depth := 0; depth < maxReExportDepth && len(queue) > 0; depth++ {
		var next []step
		for _, s := range queue {
			var src []byte
			var srcPath string
			for _, c := range fileCandidatesFor(s.file) {
				if data := mi.cachedSource(c, srcCache); len(data) > 0 {
					src, srcPath = data, c
					break
				}
			}
			if len(src) == 0 {
				continue
			}
			for _, re := range mi.reExportsFor(string(src), srcPath) {
				var want string
				if re.star {
					want = s.name
				} else if orig, ok := re.names[s.name]; ok {
					want = orig
				}
				if want == "" {
					continue
				}
				for _, c := range fileCandidatesFor(re.fromFile) {
					reachable[c] = true
				}
				st := step{file: re.fromFile, name: want}
				if !seen[st] {
					seen[st] = true
					next = append(next, st)
				}
			}
		}
		queue = next
	}
	return reachable
}

// isImportResolvableLang reports whether the contract source file
// uses an import system this resolver can parse. TypeScript and
// JavaScript files use ES-module imports we understand; Rust `use`
// declarations are parsed for `pub use` re-export following. Go uses
// package qualification which the in-file pass already handles
// (and would have produced an unambiguous resolution at extraction
// time).
func isImportResolvableLang(filePath string) bool {
	switch path.Ext(filePath) {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs", ".rs":
		return true
	}
	return false
}

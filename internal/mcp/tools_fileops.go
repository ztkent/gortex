package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/elide"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

// errPathUnresolved is returned when a relative path cannot be anchored to any
// indexed repo. Callers should surface this as a clear error rather than letting
// os.Open resolve it against the daemon process CWD, which is unrelated to any
// repo and silently produces wrong results.
var errPathUnresolved = errors.New("path is not absolute and no indexed repo could anchor it")

// errPathEscape is returned when a relative or repo-prefixed path resolves
// outside the anchor repo's root via `..` traversal. Absolute paths bypass
// this check by design — agents that hand over an absolute path are
// responsible for the location.
var errPathEscape = errors.New("relative path escapes the indexed repo root")

// resolveFilePath turns a user-supplied path into the absolute filesystem
// path the write should target. Accepts:
//   - absolute paths, used as-is (no containment check)
//   - repo-prefixed paths (e.g. "gortex/internal/foo.go" in multi-repo mode)
//   - paths relative to the single indexer's root (single-repo mode only)
//
// Returns the absolute path, the repo-relative form for session
// bookkeeping, and an error sentinel describing WHY the input was
// rejected when one of the rejection paths fires:
//
//   - errPathUnresolved — empty input, missing indexer, or a multi-repo
//     bare-relative path that doesn't match any registered prefix
//     (no implicit "primary repo", so the daemon refuses to fall
//     through to its process CWD).
//   - errPathEscape — relative or repo-prefixed input that resolves
//     outside the anchor repo's root via `..` segments. Catches
//     `../../etc/passwd` style attempts at the boundary instead of
//     letting them silently land on system files.
//
// Absolute paths bypass containment by design — agents that hand
// over an absolute path own the location.
func (s *Server) resolveFilePath(rawPath string) (absPath, relPath string, err error) {
	if rawPath == "" {
		return "", "", fmt.Errorf("%w: path is empty", errPathUnresolved)
	}

	if filepath.IsAbs(rawPath) {
		abs := filepath.Clean(rawPath)
		return abs, s.repoRelative(abs), nil
	}

	if s.multiIndexer != nil {
		// Multi-repo mode requires a repo-prefixed path. Bare-relative
		// paths are ambiguous; refuse rather than fall through to the
		// daemon process CWD.
		prefix := matchedRepoPrefix(s.multiIndexer, rawPath)
		if prefix == "" {
			prefixes := s.multiIndexer.RepoPrefixes()
			return "", "", fmt.Errorf("%w: path %q does not start with a known repo prefix; expected one of: %s/, or an absolute path",
				errPathUnresolved, rawPath, strings.Join(prefixes, "/, "))
		}
		root, ok := s.multiIndexer.RepoRoot(prefix)
		if !ok {
			return "", "", fmt.Errorf("%w: repo prefix %q has no root path", errPathUnresolved, prefix)
		}
		abs := filepath.Clean(filepath.Join(root, strings.TrimPrefix(rawPath, prefix+"/")))
		if !pathContainedIn(abs, root) {
			return "", "", fmt.Errorf("%w: %q resolves to %q, outside repo root %q", errPathEscape, rawPath, abs, root)
		}
		// Re-root the write into the linked worktree the file belongs
		// to when the resolved root is a main checkout that shares its
		// index identity with one. relPath stays the repo-prefixed form
		// for session bookkeeping — the prefix names the same repo
		// regardless of which worktree the bytes land in.
		abs = worktreeRootedPath(abs, root, s.multiIndexer)
		return abs, rawPath, nil
	}

	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			abs := filepath.Clean(filepath.Join(root, rawPath))
			if !pathContainedIn(abs, root) {
				return "", "", fmt.Errorf("%w: %q resolves to %q, outside repo root %q", errPathEscape, rawPath, abs, root)
			}
			return abs, rawPath, nil
		}
	}

	return "", "", fmt.Errorf("%w: no indexer is attached and path %q is not absolute", errPathUnresolved, rawPath)
}

// matchedRepoPrefix returns the longest repo prefix that prefixes rawPath
// (with a "/" separator) in the multi-indexer. Used to decide which repo
// anchors a repo-prefixed path before joining with its root.
func matchedRepoPrefix(mi multiRepoLookup, rawPath string) string {
	if mi == nil || rawPath == "" {
		return ""
	}
	best := ""
	for _, prefix := range mi.RepoPrefixes() {
		if prefix == "" {
			continue
		}
		// Longest match wins so a nested repo (prefix "a/b") is chosen
		// over its parent (prefix "a") for a path under the child,
		// independent of RepoPrefixes() iteration order.
		if strings.HasPrefix(rawPath, prefix+"/") && len(prefix) > len(best) {
			best = prefix
		}
	}
	return best
}

// multiRepoLookup is the subset of *MultiIndexer that resolveFilePath
// needs. Pulled out as an interface for testability and to keep the
// resolver decoupled from the broader MultiIndexer surface.
type multiRepoLookup interface {
	RepoPrefixes() []string
	RepoRoot(prefix string) (string, bool)
	// LinkedWorktreeRoots returns the on-disk roots of every tracked
	// linked git worktree that shares a .git common directory with the
	// checkout at the given path.
	LinkedWorktreeRoots(mainRepoPath string) []string
}

// worktreeRootedPath re-roots an edit target into the linked git
// worktree the file actually belongs to. All worktrees of one repo
// reuse a single index identity, so a repo-relative path resolved
// against one checkout's root (root → abs) can name a file that
// physically lives in a sibling worktree. When the resolved root is a
// main checkout, abs does not exist there, and exactly one tracked
// linked worktree of that repo *does* contain the same repo-relative
// file, the write is re-rooted into that worktree — so editing a file
// in a linked worktree modifies the worktree's copy, not the main
// repo's.
//
// The function is deliberately conservative:
//   - It never moves a path that already exists at abs — the resolved
//     root genuinely owns the file.
//   - It never moves a path when the resolved root is itself a linked
//     worktree — abs is already inside the right checkout.
//   - It never moves a brand-new file (one that exists in no checkout):
//     a fresh write_file lands under the prefix the caller named.
//   - It re-roots only when the match is unambiguous (exactly one
//     worktree contains the file); two candidates leave abs untouched.
func worktreeRootedPath(abs, root string, mi multiRepoLookup) string {
	if abs == "" || root == "" || mi == nil {
		return abs
	}
	// Already inside a linked worktree — nothing to re-root.
	if indexer.ResolveWorktree(root).IsWorktree {
		return abs
	}
	// The file is physically present where it resolved — the resolved
	// root owns it.
	if _, err := os.Stat(abs); err == nil {
		return abs
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return abs
	}
	match := ""
	for _, wt := range mi.LinkedWorktreeRoots(root) {
		candidate := filepath.Clean(filepath.Join(wt, rel))
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		if match != "" && match != candidate {
			// Ambiguous — more than one worktree carries this file.
			// Leave the path at its originally-resolved location.
			return abs
		}
		match = candidate
	}
	if match != "" {
		return match
	}
	return abs
}

// pathContainedIn reports whether abs sits at or beneath root, after
// cleaning both. The check is purely lexical — it doesn't dereference
// symlinks — but combined with filepath.Clean it catches the standard
// `..` traversal class. A defense-in-depth pass for symlink-target
// containment is left to the OS-level rename, which fails when the
// destination resolves to a different filesystem object.
func pathContainedIn(abs, root string) bool {
	if abs == "" || root == "" {
		return false
	}
	abs = filepath.Clean(abs)
	root = filepath.Clean(root)
	if abs == root {
		return true
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}

// resolveNodePath returns the absolute filesystem path for a graph node.
// Uses node.RepoPrefix to find the owning repo's root in multi-repo mode;
// falls back to the lone indexer's RootPath in single-repo mode. Returns an
// error (not a relative path) when no repo root is available, to keep callers
// from passing a bare-relative path to os.Open and resolving against the
// daemon process CWD.
func (s *Server) resolveNodePath(node *graph.Node) (string, error) {
	if node == nil {
		return "", errors.New("nil node")
	}
	if node.FilePath == "" {
		return "", fmt.Errorf("node %q has no file path", node.ID)
	}
	if filepath.IsAbs(node.FilePath) {
		return filepath.Clean(node.FilePath), nil
	}
	if s.multiIndexer != nil {
		if root, ok := s.multiIndexer.RepoRoot(node.RepoPrefix); ok {
			// applyRepoPrefix stamps `<repoPrefix>/` onto node.FilePath
			// at index time, so a node's FilePath looks like
			// `gortex/internal/exporter/cypher.go`. RepoRoot returns
			// the on-disk path that ALREADY corresponds to the repo
			// (e.g. `/Users/zzet/code/my/gortex/gortex`). Joining as-is
			// duplicates the prefix segment when the repo's basename
			// matches the prefix — strip the leading `<prefix>/` from
			// the file path before joining so the result is the real
			// on-disk file regardless of basename collision.
			rel := node.FilePath
			if node.RepoPrefix != "" {
				rel = strings.TrimPrefix(rel, node.RepoPrefix+"/")
			}
			abs := filepath.Clean(filepath.Join(root, rel))
			// Re-root onto the linked worktree the file belongs to —
			// same reasoning as resolveFilePath: worktrees of one repo
			// share an index identity, so a node's resolved path can
			// land on a sibling checkout.
			return worktreeRootedPath(abs, root, s.multiIndexer), nil
		}
		return "", fmt.Errorf("could not resolve repo root for node %q (repo_prefix=%q)", node.ID, node.RepoPrefix)
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			return filepath.Clean(filepath.Join(root, node.FilePath)), nil
		}
	}
	return "", fmt.Errorf("%w: node=%q file=%q", errPathUnresolved, node.ID, node.FilePath)
}

// withAbsPath returns a shallow copy of n with AbsoluteFilePath populated
// from the indexer roots. The canonical graph node is never mutated, so
// this is safe to call from concurrent request handlers; AbsoluteFilePath
// is left empty when the path cannot be resolved (callers still carry the
// repo-relative file_path).
func (s *Server) withAbsPath(n *graph.Node) *graph.Node {
	if n == nil {
		return nil
	}
	cp := *n
	if abs, err := s.resolveNodePath(n); err == nil {
		cp.AbsoluteFilePath = abs
	}
	return &cp
}

// withAbsPaths maps withAbsPath over a slice, returning a fresh slice of
// copies. The input slice and its nodes are left untouched.
func (s *Server) withAbsPaths(nodes []*graph.Node) []*graph.Node {
	if nodes == nil {
		return nil
	}
	out := make([]*graph.Node, len(nodes))
	for i, n := range nodes {
		out[i] = s.withAbsPath(n)
	}
	return out
}

// resolveGraphPath returns the absolute filesystem path for a repo-prefixed
// graph path (e.g. "gortex/internal/foo.go"). Mirrors resolveNodePath but
// works on raw path strings — used for edges, search results, and other
// references that don't carry a Node pointer. Returns errPathUnresolved
// rather than letting os.Open resolve against the daemon process CWD.
func (s *Server) resolveGraphPath(graphPath string) (string, error) {
	if graphPath == "" {
		return "", errors.New("empty path")
	}
	if filepath.IsAbs(graphPath) {
		return filepath.Clean(graphPath), nil
	}
	if s.multiIndexer != nil {
		if abs := s.multiIndexer.ResolveFilePath(graphPath); abs != "" {
			abs = filepath.Clean(abs)
			// Re-root onto the linked worktree the file belongs to.
			// ResolveFilePath joins against the matched prefix's root;
			// recover that root so worktreeRootedPath can decide.
			if prefix := matchedRepoPrefix(s.multiIndexer, graphPath); prefix != "" {
				if root, ok := s.multiIndexer.RepoRoot(prefix); ok {
					abs = worktreeRootedPath(abs, root, s.multiIndexer)
				}
			}
			return abs, nil
		}
		return "", fmt.Errorf("could not resolve repo root for path %q", graphPath)
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			return filepath.Clean(filepath.Join(root, graphPath)), nil
		}
	}
	return "", fmt.Errorf("%w: path=%q", errPathUnresolved, graphPath)
}

// repoRelative converts an absolute path to a repo-prefixed or root-relative
// string if it falls under any indexed repo, otherwise returns the absolute
// path unchanged.
func (s *Server) repoRelative(absPath string) string {
	if s.multiIndexer != nil {
		if prefix := s.multiIndexer.RepoForFile(absPath); prefix != "" {
			if idx := s.multiIndexer.GetIndexer(prefix); idx != nil {
				if rel, err := filepath.Rel(idx.RootPath(), absPath); err == nil {
					return filepath.ToSlash(filepath.Join(prefix, rel))
				}
			}
			return prefix
		}
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			if rel, err := filepath.Rel(root, absPath); err == nil && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel)
			}
		}
	}
	return absPath
}

// reindexFile refreshes the graph for a single file after a write. Best-effort:
// non-source files or files outside any indexed repo are silently skipped.
func (s *Server) reindexFile(absPath string) bool {
	if s.multiIndexer != nil {
		if prefix := s.multiIndexer.RepoForFile(absPath); prefix != "" {
			if idx := s.multiIndexer.GetIndexer(prefix); idx != nil {
				if err := idx.IndexFile(absPath); err == nil {
					return true
				}
			}
		}
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			if rel, err := filepath.Rel(root, absPath); err == nil && !strings.HasPrefix(rel, "..") {
				if err := s.indexer.IndexFile(absPath); err == nil {
					return true
				}
			}
		}
	}
	return false
}

func (s *Server) handleEditFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rawPath, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	oldString, err := req.RequireString("old_string")
	if err != nil {
		return mcp.NewToolResultError("old_string is required"), nil
	}
	newString, err := req.RequireString("new_string")
	if err != nil {
		return mcp.NewToolResultError("new_string is required"), nil
	}
	if oldString == newString {
		return mcp.NewToolResultError("old_string and new_string are identical"), nil
	}
	replaceAll := req.GetBool("replace_all", false)
	dryRun := req.GetBool("dry_run", false)

	absPath, relPath, resolveErr := s.resolveFilePath(rawPath)
	if resolveErr != nil {
		return mcp.NewToolResultError(resolveErr.Error()), nil
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not read file: %v", err)), nil
	}
	fileStr := string(content)

	count := strings.Count(fileStr, oldString)
	if count == 0 {
		return mcp.NewToolResultError(
			"old_string not found in file. Use get_file_summary or get_editing_context to inspect the current content."), nil
	}
	if count > 1 && !replaceAll {
		hint := matchLocationsHint(fileStr, oldString)
		return mcp.NewToolResultError(fmt.Sprintf(
			"old_string matches %d locations%s. Provide a larger fragment for uniqueness or pass replace_all=true.", count, hint)), nil
	}

	var newContent string
	var replacements int
	if replaceAll {
		newContent = strings.ReplaceAll(fileStr, oldString, newString)
		replacements = count
	} else {
		newContent = strings.Replace(fileStr, oldString, newString, 1)
		replacements = 1
	}

	if dryRun {
		// Dry-run: validate everything but skip the write + reindex.
		// Returns the same shape so callers can branch on dry_run for a
		// preview before committing.
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"path":          relPath,
			"status":        "would_apply",
			"dry_run":       true,
			"replacements":  replacements,
			"bytes_written": len(newContent),
			"reindexed":     false,
		})
	}

	perm := os.FileMode(0o644)
	if info, err := os.Stat(absPath); err == nil {
		perm = info.Mode().Perm()
	}
	if err := agents.AtomicWriteFile(absPath, []byte(newContent), perm); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not write file: %v", err)), nil
	}

	sess := s.sessionFor(ctx)
	sess.recordModified(relPath)

	reindexed := s.reindexFile(absPath)

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"path":          relPath,
		"status":        "applied",
		"replacements":  replacements,
		"bytes_written": len(newContent),
		"reindexed":     reindexed,
	})
}

func (s *Server) handleWriteFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rawPath, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	content, err := req.RequireString("content")
	if err != nil {
		return mcp.NewToolResultError("content is required"), nil
	}
	dryRun := req.GetBool("dry_run", false)

	absPath, relPath, resolveErr := s.resolveFilePath(rawPath)
	if resolveErr != nil {
		return mcp.NewToolResultError(resolveErr.Error()), nil
	}

	status := "created"
	perm := os.FileMode(0o644)
	if info, err := os.Stat(absPath); err == nil {
		if info.IsDir() {
			return mcp.NewToolResultError(fmt.Sprintf("path %q is a directory", rawPath)), nil
		}
		status = "overwritten"
		perm = info.Mode().Perm()
	}

	if dryRun {
		// Dry-run: skip the write + reindex but report what would happen.
		dryStatus := "would_create"
		if status == "overwritten" {
			dryStatus = "would_overwrite"
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"path":          relPath,
			"status":        dryStatus,
			"dry_run":       true,
			"bytes_written": len(content),
			"reindexed":     false,
		})
	}

	if err := agents.AtomicWriteFile(absPath, []byte(content), perm); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not write file: %v", err)), nil
	}

	sess := s.sessionFor(ctx)
	sess.recordModified(relPath)

	reindexed := s.reindexFile(absPath)

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"path":          relPath,
		"status":        status,
		"bytes_written": len(content),
		"reindexed":     reindexed,
	})
}

// matchLocationsHint returns a brief " (lines X, Y, Z)" hint listing up to
// three line numbers where oldString matches in fileStr. Empty when there
// are zero matches. Helps an agent choose a more unique fragment without
// re-reading the file.
func matchLocationsHint(fileStr, oldString string) string {
	if oldString == "" {
		return ""
	}
	const maxHits = 3
	lines := []int{}
	offset := 0
	for offset < len(fileStr) {
		idx := strings.Index(fileStr[offset:], oldString)
		if idx < 0 {
			break
		}
		absIdx := offset + idx
		// Line number = 1 + count of '\n' before absIdx.
		line := 1 + strings.Count(fileStr[:absIdx], "\n")
		lines = append(lines, line)
		if len(lines) >= maxHits {
			break
		}
		offset = absIdx + len(oldString)
		if len(oldString) == 0 {
			offset++
		}
	}
	if len(lines) == 0 {
		return ""
	}
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = fmt.Sprintf("%d", l)
	}
	suffix := ""
	if strings.Count(fileStr, oldString) > maxHits {
		suffix = ", ..."
	}
	return fmt.Sprintf(" (first match lines %s%s)", strings.Join(parts, ", "), suffix)
}

// handleReadFile returns the full content of a file as a string,
// optionally rewritten through the tree-sitter elider when
// compress_bodies=true. Path resolution shares the same rules as
// edit_file / write_file (absolute, repo-prefixed, or
// single-repo-root-relative); the file does not need to be indexed.
func (s *Server) handleReadFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rawPath, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	absPath, relPath, resolveErr := s.resolveFilePath(rawPath)
	if resolveErr != nil {
		return mcp.NewToolResultError(resolveErr.Error()), nil
	}
	info, statErr := os.Stat(absPath)
	if statErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not stat file: %v", statErr)), nil
	}
	if info.IsDir() {
		return mcp.NewToolResultError(fmt.Sprintf("path %q is a directory", rawPath)), nil
	}
	// Honour the editor-buffer overlay if one is active for this path.
	var content []byte
	if buf, ok := s.overlayContentFor(ctx, absPath); ok {
		content = []byte(buf)
	} else {
		b, rerr := os.ReadFile(absPath)
		if rerr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not read file: %v", rerr)), nil
		}
		content = b
	}

	originalBytes := len(content)
	isBinary := looksBinary(content)
	bodiesElided := false
	var keptSymbols []string
	language := s.detectLanguageForPath(ctx, absPath, relPath)
	// File symbols power both the `keep` predicate and frecency credit.
	sg := s.engineFor(ctx).GetFileSymbols(relPath)
	if req.GetBool("compress_bodies", false) && language != "" && elide.IsSupported(language) {
		var symbols []*graph.Node
		if sg != nil {
			symbols = sg.Nodes
		}
		keepPred, resolved := resolveKeepPredicate(req.GetString("keep", ""), symbols)
		if out, eerr := elide.CompressWith(content, language, elide.Options{Keep: keepPred}); eerr == nil && len(out) != len(content) {
			content = out
			bodiesElided = true
			keptSymbols = resolved
		}
	}

	// Salience truncation: collapse leaf-statement runs (and head-cut
	// non-code files) when the file is still over the max_lines budget.
	salienceTruncated := false
	if maxLines := req.GetInt("max_lines", 0); maxLines > 0 {
		if out, truncated, _ := elide.SalienceTruncate(content, language, maxLines); truncated {
			content = out
			salienceTruncated = true
		}
	}

	// Record the access for frecency credit on any node defined in
	// this file. read_file is a heavy access (full file), so we
	// credit every defined symbol — keeps the "agent is working in
	// this area" signal aligned with how the agent burned its
	// budget.
	s.sessionFor(ctx).recordFile(relPath)
	if sg != nil {
		for _, n := range sg.Nodes {
			if n == nil || n.Kind == graph.KindFile {
				continue
			}
			s.frecency.Record(n.ID)
		}
	}

	result := map[string]any{
		"path":           relPath,
		"language":       language,
		"bytes":          len(content),
		"original_bytes": originalBytes,
		"content":        string(content),
	}
	if bodiesElided {
		result["bodies_elided"] = true
		if len(keptSymbols) > 0 {
			result["kept_symbols"] = keptSymbols
		}
	}
	if salienceTruncated {
		result["salience_truncated"] = true
	}

	// Omission notes: tell the model what the payload deliberately
	// leaves out or reshapes, so it does not reason about absent code.
	omissions := pathOmissions(relPath)
	if isBinary {
		omissions = append(omissions, omission("binary",
			"file is binary — the content field holds raw bytes, not source text"))
	}
	if bodiesElided {
		omissions = append(omissions, omission("compressed",
			"function and method bodies replaced with elided stubs; signatures and structure kept"))
	}
	if salienceTruncated {
		omissions = append(omissions, omission("truncated",
			"oversized source reduced toward its control-flow skeleton; runs of leaf statements collapsed"))
	}
	if len(omissions) > 0 {
		result["omissions"] = omissions
	}

	etag := computeETag(result)
	if ifNoneMatch := req.GetString("if_none_match", ""); ifNoneMatch != "" && ifNoneMatch == etag {
		return notModifiedResult(etag), nil
	}
	result["etag"] = etag

	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// detectLanguageForPath resolves the language code for a file. Prefers
// the indexed file node's Node.Language (canonical: same code the
// extractor stamped at index time). Falls back to the parser
// Registry's extension-based detection so unindexed files (or files
// outside any tracked repo) still get a language tag.
func (s *Server) detectLanguageForPath(ctx context.Context, absPath, relPath string) string {
	// Try the indexed file node first.
	if sg := s.engineFor(ctx).GetFileSymbols(relPath); sg != nil {
		for _, n := range sg.Nodes {
			if n != nil && n.Kind == graph.KindFile && n.Language != "" {
				return n.Language
			}
		}
	}
	// Fall back to the parser registry from whichever indexer owns
	// the file. In multi-repo mode, every indexer holds the same
	// registry instance; in single-repo mode we ask the lone indexer.
	//
	// A bounded prefix read lets the registry's content probe place
	// an ambiguous extension (.h, .m) or an unknown-extension script.
	var head []byte
	if f, err := os.Open(absPath); err == nil {
		buf := make([]byte, 512)
		if n, _ := f.Read(buf); n > 0 {
			head = buf[:n]
		}
		_ = f.Close()
	}
	if s.multiIndexer != nil {
		for _, prefix := range s.multiIndexer.RepoPrefixes() {
			if idx := s.multiIndexer.GetIndexer(prefix); idx != nil {
				if reg := idx.Registry(); reg != nil {
					if lang, ok := reg.DetectLanguageContent(absPath, head); ok {
						return lang
					}
				}
			}
		}
	}
	if s.indexer != nil {
		if reg := s.indexer.Registry(); reg != nil {
			if lang, ok := reg.DetectLanguageContent(absPath, head); ok {
				return lang
			}
		}
	}
	return ""
}

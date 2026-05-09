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
	"github.com/zzet/gortex/internal/graph"
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
	for _, prefix := range mi.RepoPrefixes() {
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(rawPath, prefix+"/") {
			return prefix
		}
	}
	return ""
}

// multiRepoLookup is the subset of *MultiIndexer that resolveFilePath
// needs. Pulled out as an interface for testability and to keep the
// resolver decoupled from the broader MultiIndexer surface.
type multiRepoLookup interface {
	RepoPrefixes() []string
	RepoRoot(prefix string) (string, bool)
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
			return filepath.Clean(filepath.Join(root, rel)), nil
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
			return filepath.Clean(abs), nil
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

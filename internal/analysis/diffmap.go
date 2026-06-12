package analysis

import (
	"bufio"
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph"
)

// DiffHunk represents a changed range in a file.
type DiffHunk struct {
	FilePath  string `json:"file_path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// ChangedSymbol is a symbol affected by a git diff hunk.
type ChangedSymbol struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	FilePath string `json:"file_path"`
	Line     int    `json:"start_line"`
}

// DiffResult is the output of git diff → symbol mapping.
type DiffResult struct {
	Hunks          []DiffHunk      `json:"hunks"`
	ChangedSymbols []ChangedSymbol `json:"changed_symbols"`
	ChangedFiles   []string        `json:"changed_files"`
}

// MapGitDiff parses git diff output and maps changed lines to symbols in the graph.
// scope: "unstaged", "staged", "all", "compare"
// baseRef: used when scope is "compare" (e.g., "main")
// repoRoot: absolute path to the repository root
// repoPrefix: the graph repo prefix anchoring repoRoot's indexed nodes.
// Multi-repo daemons key file paths as "<prefix>/<rel>" while git emits
// repo-relative paths; empty in single-repo / unprefixed mode.
func MapGitDiff(g graph.Store, repoRoot, repoPrefix, scope, baseRef string) (*DiffResult, error) {
	args := buildDiffArgs(scope, baseRef)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// gitcmd runs `git -C repoRoot args...`; use Run (raw stdout, no trailing
	// trim) so the parsed diff stays byte-identical to the pre-gitcmd output.
	output, err := gitcmd.Run(ctx, repoRoot, args...)
	if err != nil {
		// If git diff returns empty, that's fine
		if len(output) == 0 {
			return &DiffResult{}, nil
		}
		return nil, fmt.Errorf("git diff failed: %w", err)
	}

	hunks := parseDiffHunks(string(output))
	return joinHunksToSymbols(g, repoPrefix, hunks), nil
}

// JoinFileNodes maps one repo-relative changed-file path to the graph nodes
// its file defines. Indexed file paths carry the repo prefix in multi-repo
// mode ("<prefix>/<rel>") while git and forge APIs emit repo-relative paths,
// so the raw lookup is tried first (single-repo / already-prefixed input)
// and the prefixed form second.
func JoinFileNodes(g graph.Store, repoPrefix, path string) []*graph.Node {
	if nodes := g.GetFileNodes(path); len(nodes) > 0 {
		return nodes
	}
	if repoPrefix == "" || strings.HasPrefix(path, repoPrefix+"/") {
		return nil
	}
	return g.GetFileNodes(repoPrefix + "/" + path)
}

// JoinFilePath returns the graph-keyed variant of a repo-relative file path:
// the raw path when it resolves (or no prefix applies), otherwise the prefixed
// form when that resolves. Falls back to the raw path when neither does, so
// the caller's downstream lookup misses exactly as it would have anyway.
func JoinFilePath(g graph.Store, repoPrefix, path string) string {
	if repoPrefix == "" || strings.HasPrefix(path, repoPrefix+"/") {
		return path
	}
	if len(g.GetFileNodes(path)) > 0 {
		return path
	}
	if prefixed := repoPrefix + "/" + path; len(g.GetFileNodes(prefixed)) > 0 {
		return prefixed
	}
	return path
}

// joinHunksToSymbols builds the hunk→symbol/file join shared by MapGitDiff
// and MapGitDiffWithLines: every symbol whose line range overlaps a changed
// hunk in its file, deduped, plus the changed-file set. ChangedFiles keeps
// the diff-relative paths (callers re-join them with git pathspecs); only
// the node lookup is prefix-aware.
func joinHunksToSymbols(g graph.Store, repoPrefix string, hunks []DiffHunk) *DiffResult {
	result := &DiffResult{Hunks: hunks}

	fileSet := make(map[string]bool)
	symbolSeen := make(map[string]bool)

	for _, hunk := range hunks {
		fileSet[hunk.FilePath] = true

		// Find symbols whose line range overlaps the hunk
		for _, n := range JoinFileNodes(g, repoPrefix, hunk.FilePath) {
			if n.Kind == graph.KindFile {
				continue
			}
			// Check if symbol's line range overlaps with the hunk
			if n.StartLine <= hunk.EndLine && n.EndLine >= hunk.StartLine {
				if !symbolSeen[n.ID] {
					symbolSeen[n.ID] = true
					result.ChangedSymbols = append(result.ChangedSymbols, ChangedSymbol{
						ID:       n.ID,
						Name:     n.Name,
						Kind:     string(n.Kind),
						FilePath: n.FilePath,
						Line:     n.StartLine,
					})
				}
			}
		}
	}

	for f := range fileSet {
		result.ChangedFiles = append(result.ChangedFiles, f)
	}

	return result
}

// GitDiffArgs builds the `git diff` argv for a scope ("unstaged", "staged",
// "all", "compare") with the given context width. The -c overrides pin the
// diff header prefixes to git's standard a/ b/ form: parseDiffHunks and
// parseDiffLines anchor on "+++ b/", which a developer's diff.mnemonicPrefix
// (c/ w/ headers on worktree-side diffs) or diff.noprefix config would
// otherwise silently zero out — every hunk drops, every diff-driven tool
// reports an empty changeset.
func GitDiffArgs(scope, baseRef string, unified int) []string {
	args := []string{
		"-c", "diff.mnemonicPrefix=false",
		"-c", "diff.noprefix=false",
		"diff",
	}
	switch scope {
	case "staged":
		args = append(args, "--cached")
	case "all":
		args = append(args, "HEAD")
	case "compare":
		if baseRef == "" {
			baseRef = "main"
		}
		args = append(args, baseRef+"...HEAD")
	default: // unstaged — bare `git diff`
	}
	return append(args, fmt.Sprintf("--unified=%d", unified))
}

func buildDiffArgs(scope, baseRef string) []string {
	return GitDiffArgs(scope, baseRef, 0)
}

// buildDiffArgsWithContext mirrors buildDiffArgs but emits a context window so
// the new-side line text survives into the hunk body for snippet grounding.
func buildDiffArgsWithContext(scope, baseRef string) []string {
	return GitDiffArgs(scope, baseRef, 3)
}

// HunkLine is a single new-side line carried out of a unified diff: added lines
// (Side "+") and context lines (Side " "), each tagged with its line number in
// the post-change file. Removed lines never appear (they have no new-side line).
type HunkLine struct {
	NewLine int    `json:"new_line"`
	Side    string `json:"side"`
	Text    string `json:"text"`
}

// ParseDiffHunks parses unified git-diff output into per-file changed ranges.
// It is the exported entry point over the same parser MapGitDiff uses and
// returns results identical to the internal parser.
func ParseDiffHunks(output string) []DiffHunk {
	return parseDiffHunks(output)
}

// parseDiffLines walks unified diff output and returns, per new-side file path,
// the added and context lines with their post-change line numbers. Removed
// lines are skipped; the new-side line counter only advances on added/context.
// Keys match DiffHunk.FilePath (cleaned, relative) so a hunk and its lines join.
func parseDiffLines(output string) map[string][]HunkLine {
	lines := make(map[string][]HunkLine)
	var currentFile string
	var newLine int

	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "+++ b/") {
			currentFile = filepath.Clean(strings.TrimPrefix(line, "+++ b/"))
			newLine = 0
			continue
		}
		if strings.HasPrefix(line, "+++ /dev/null") {
			currentFile = ""
			newLine = 0
			continue
		}
		// Skip the remaining diff-header lines so their leading +/-/space never
		// leaks into the hunk body.
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "diff ") ||
			strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "new file") ||
			strings.HasPrefix(line, "deleted file") || strings.HasPrefix(line, "rename ") ||
			strings.HasPrefix(line, "similarity ") || strings.HasPrefix(line, "old mode") ||
			strings.HasPrefix(line, "new mode") || strings.HasPrefix(line, "copy ") {
			continue
		}

		if strings.HasPrefix(line, "@@") {
			if currentFile == "" {
				continue
			}
			start, ok := parseNewStart(line)
			if !ok {
				continue
			}
			newLine = start
			continue
		}

		if currentFile == "" || newLine == 0 {
			continue
		}

		switch {
		case strings.HasPrefix(line, "+"):
			lines[currentFile] = append(lines[currentFile], HunkLine{
				NewLine: newLine,
				Side:    "+",
				Text:    line[1:],
			})
			newLine++
		case strings.HasPrefix(line, "-"):
			// Removed line — no new-side position, do not advance.
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file" marker — not a content line.
		default:
			// Context line (leading space), or a bare blank context line.
			text := line
			if strings.HasPrefix(line, " ") {
				text = line[1:]
			}
			lines[currentFile] = append(lines[currentFile], HunkLine{
				NewLine: newLine,
				Side:    " ",
				Text:    text,
			})
			newLine++
		}
	}

	return lines
}

// parseNewStart extracts the new-side starting line from a "@@ -a,b +c,d @@"
// hunk header.
func parseNewStart(line string) (int, bool) {
	parts := strings.SplitN(line, "@@", 3)
	if len(parts) < 2 {
		return 0, false
	}
	for _, f := range strings.Fields(strings.TrimSpace(parts[1])) {
		if !strings.HasPrefix(f, "+") {
			continue
		}
		f = strings.TrimPrefix(f, "+")
		rangeP := strings.SplitN(f, ",", 2)
		start, err := strconv.Atoi(rangeP[0])
		if err != nil {
			return 0, false
		}
		return start, true
	}
	return 0, false
}

// MapGitDiffWithLines mirrors MapGitDiff but uses a context-bearing diff so it
// can additionally return, per file, the new-side lines (added + context) with
// their post-change line numbers — the substrate snippet grounding anchors on.
// The returned *DiffResult is computed with the same logic as MapGitDiff (only
// the diff's context width differs), so symbol overlap is unaffected.
// repoPrefix anchors the node join exactly as in MapGitDiff.
func MapGitDiffWithLines(g graph.Store, repoRoot, repoPrefix, scope, baseRef string) (*DiffResult, map[string][]HunkLine, error) {
	args := buildDiffArgsWithContext(scope, baseRef)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Run (raw stdout) keeps the trailing newline so the final hunk line is
	// never dropped; gitcmd injects `-C repoRoot` for us.
	output, err := gitcmd.Run(ctx, repoRoot, args...)
	if err != nil {
		if len(output) == 0 {
			return &DiffResult{}, map[string][]HunkLine{}, nil
		}
		return nil, nil, fmt.Errorf("git diff failed: %w", err)
	}

	text := string(output)
	hunks := parseDiffHunks(text)
	lines := parseDiffLines(text)

	return joinHunksToSymbols(g, repoPrefix, hunks), lines, nil
}

func parseDiffHunks(output string) []DiffHunk {
	var hunks []DiffHunk
	var currentFile string

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()

		// Detect file path from diff header
		if strings.HasPrefix(line, "+++ b/") {
			currentFile = strings.TrimPrefix(line, "+++ b/")
			continue
		}
		if strings.HasPrefix(line, "+++ /dev/null") {
			currentFile = ""
			continue
		}

		// Parse @@ hunk header for the new file's line range
		if strings.HasPrefix(line, "@@") && currentFile != "" {
			hunk := parseHunkHeader(line, currentFile)
			if hunk != nil {
				hunks = append(hunks, *hunk)
			}
		}
	}

	return hunks
}

func parseHunkHeader(line, filePath string) *DiffHunk {
	// Format: @@ -old,count +new,count @@
	parts := strings.SplitN(line, "@@", 3)
	if len(parts) < 2 {
		return nil
	}
	ranges := strings.TrimSpace(parts[1])
	fields := strings.Fields(ranges)

	for _, f := range fields {
		if strings.HasPrefix(f, "+") {
			f = strings.TrimPrefix(f, "+")
			rangeP := strings.SplitN(f, ",", 2)
			start, err := strconv.Atoi(rangeP[0])
			if err != nil {
				continue
			}
			count := 1
			if len(rangeP) > 1 {
				count, _ = strconv.Atoi(rangeP[1])
			}
			if count == 0 {
				count = 1
			}

			// Normalize file path to be relative
			relPath := filepath.Clean(filePath)

			return &DiffHunk{
				FilePath:  relPath,
				StartLine: start,
				EndLine:   start + count - 1,
			}
		}
	}
	return nil
}

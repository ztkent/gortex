package analysis

import (
	"bufio"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

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
func MapGitDiff(g *graph.Graph, repoRoot, scope, baseRef string) (*DiffResult, error) {
	args := buildDiffArgs(scope, baseRef)
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot

	output, err := cmd.Output()
	if err != nil {
		// If git diff returns empty, that's fine
		if len(output) == 0 {
			return &DiffResult{}, nil
		}
		return nil, fmt.Errorf("git diff failed: %w", err)
	}

	hunks := parseDiffHunks(string(output))

	result := &DiffResult{
		Hunks: hunks,
	}

	fileSet := make(map[string]bool)
	symbolSeen := make(map[string]bool)

	for _, hunk := range hunks {
		fileSet[hunk.FilePath] = true

		// Find symbols whose line range overlaps the hunk
		nodes := g.GetFileNodes(hunk.FilePath)
		for _, n := range nodes {
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

	return result, nil
}

func buildDiffArgs(scope, baseRef string) []string {
	switch scope {
	case "staged":
		return []string{"diff", "--cached", "--unified=0"}
	case "all":
		return []string{"diff", "HEAD", "--unified=0"}
	case "compare":
		if baseRef == "" {
			baseRef = "main"
		}
		return []string{"diff", baseRef + "...HEAD", "--unified=0"}
	default: // unstaged
		return []string{"diff", "--unified=0"}
	}
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

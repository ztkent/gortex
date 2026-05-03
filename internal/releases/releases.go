// Package releases populates per-file release metadata from git
// tag history. Each file in the graph receives meta.added_in = the
// earliest tag whose tree contained that file path. Symbols
// inherit indirectly through their file — agents asking "added
// in v1.4?" walk to the file node first.
//
// Design choices:
//
//   - One `git ls-tree -r` per tag rather than per file. Tag tree
//     enumeration is fast (O(tag-tree-size) and amortised by git's
//     packfile structure); the per-file equivalent (`git log
//     --reverse -- <file>`) would multiply runtime by the file
//     count.
//
//   - Earliest-tag-wins policy: walk tags in ascending creator-
//     date order, mark each file's added_in only on first sight.
//     A file deleted then re-added later in history thus shows the
//     first-add tag, which matches what an agent asking "when did
//     this code first ship?" expects.
//
//   - Tags without a file present produce no entry — releases that
//     happened before the file existed are not relevant to it.
package releases

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// ListTags returns the repo's tags ordered by creator date, oldest
// first. Empty slice when the repo has no tags or git is
// unavailable. Errors silently produce an empty list — releases
// enrichment is best-effort like blame.
func ListTags(repoRoot string) []string {
	cmd := exec.Command("git", "-C", repoRoot,
		"for-each-ref", "--sort=creatordate", "--format=%(refname:short)", "refs/tags/")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var tags []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		t := strings.TrimSpace(scanner.Text())
		if t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// FilesAtTag returns every tracked file path present in the tag's
// tree. Paths use forward slashes (git's convention regardless of
// OS) and are repo-relative. Errors return nil so the enrichment
// loop can continue past tags with broken refs.
func FilesAtTag(repoRoot, tag string) []string {
	cmd := exec.Command("git", "-C", repoRoot, "ls-tree", "-r", "--name-only", tag)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		f := strings.TrimSpace(scanner.Text())
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

// EnrichGraph walks the repo's tags chronologically and stamps
// meta.added_in on file nodes. Returns the number of file nodes
// enriched. The single-pass design (one ls-tree per tag, mark on
// first sight) is the right shape — re-running adjusts existing
// meta.added_in only when the tree contents have changed.
//
// Errors from individual git invocations are tolerated — a broken
// ref shouldn't kill enrichment for the rest of the tag set.
func EnrichGraph(g *graph.Graph, repoRoot string) (int, error) {
	if g == nil || repoRoot == "" {
		return 0, nil
	}
	tags := ListTags(repoRoot)
	if len(tags) == 0 {
		return 0, nil
	}

	// Build a fast index from repo-relative file path → first
	// containing tag. The repo-prefix-aware match logic later
	// strips multi-repo prefixes when applying meta.
	addedIn := make(map[string]string, 1024)
	for _, tag := range tags {
		for _, f := range FilesAtTag(repoRoot, tag) {
			if _, ok := addedIn[f]; !ok {
				addedIn[f] = tag
			}
		}
	}
	if len(addedIn) == 0 {
		return 0, fmt.Errorf("no files in any tag — empty repo or invalid refs?")
	}

	enriched := 0
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindFile {
			continue
		}
		if n.FilePath == "" {
			continue
		}
		// Multi-repo file paths look like "<repo-name>/<rel>"; the
		// addedIn index keys on the repo-relative form. Try the
		// untrimmed path first, then strip the leading segment.
		path := n.FilePath
		tag, ok := addedIn[path]
		if !ok {
			if idx := strings.Index(path, "/"); idx >= 0 {
				if t, ok2 := addedIn[path[idx+1:]]; ok2 {
					tag = t
					ok = true
				}
			}
		}
		if !ok {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta["added_in"] = tag
		enriched++
	}
	return enriched, nil
}

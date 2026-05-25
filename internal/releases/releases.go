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

// ReleaseNodeID returns the canonical KindRelease node ID for the
// given tag, scoped to a repo prefix in multi-repo mode. ID convention
// matches the schema docstring on graph.KindRelease: `release::<tag>`
// in single-repo graphs and `release::<repo>::<tag>` once a prefix is
// in play. Exported so the resolver / analyzers can construct the same
// ID without re-deriving the convention.
func ReleaseNodeID(repoPrefix, tag string) string {
	if repoPrefix == "" {
		return "release::" + tag
	}
	return "release::" + repoPrefix + "::" + tag
}

// EnrichGraph walks the repo's tags chronologically and stamps
// meta.added_in on file nodes, plus materialises one KindRelease
// node per tag so the schema's `release::<tag>` ID convention is
// queryable. Returns the number of file nodes enriched. The
// single-pass design (one ls-tree per tag, mark on first sight) is
// the right shape — re-running adjusts existing meta.added_in only
// when the tree contents have changed.
//
// repoPrefix scopes the materialised KindRelease IDs in multi-repo
// graphs so two repos that both ship a "v1.0" tag don't collide on
// the same node ID. Empty repoPrefix yields the single-repo
// `release::<tag>` shape; non-empty yields `release::<prefix>::<tag>`.
//
// Errors from individual git invocations are tolerated — a broken
// ref shouldn't kill enrichment for the rest of the tag set.
func EnrichGraph(g graph.Store, repoRoot string) (int, error) {
	return EnrichGraphWithRepoPrefix(g, repoRoot, "")
}

// EnrichGraphWithRepoPrefix is the multi-repo-aware variant of
// EnrichGraph. EnrichGraph delegates to it with an empty prefix; the
// multi-repo enricher passes the per-repo prefix so KindRelease IDs
// stay collision-free across repos.
func EnrichGraphWithRepoPrefix(g graph.Store, repoRoot, repoPrefix string) (int, error) {
	if g == nil || repoRoot == "" {
		return 0, nil
	}
	tags := ListTags(repoRoot)
	if len(tags) == 0 {
		return 0, nil
	}

	// Build a fast index from repo-relative file path → first
	// containing tag. The repo-prefix-aware match logic later
	// strips multi-repo prefixes when applying meta. We also
	// remember every tag's contributing file count so the
	// KindRelease nodes can carry size metadata cheaply.
	addedIn := make(map[string]string, 1024)
	tagFileCount := make(map[string]int, len(tags))
	for _, tag := range tags {
		files := FilesAtTag(repoRoot, tag)
		tagFileCount[tag] = len(files)
		for _, f := range files {
			if _, ok := addedIn[f]; !ok {
				addedIn[f] = tag
			}
		}
	}
	if len(addedIn) == 0 {
		return 0, fmt.Errorf("no files in any tag — empty repo or invalid refs?")
	}

	// Materialise one KindRelease node per tag. Idempotent — graph
	// AddNode dedupes by ID — and incremental-safe: re-running with
	// a fresh tag set adds the new tag nodes without disturbing
	// existing ones. Indexed-position metadata lets agents ask
	// "what changed in v1.2?" with a single graph query.
	for i, tag := range tags {
		node := &graph.Node{
			ID:         ReleaseNodeID(repoPrefix, tag),
			Kind:       graph.KindRelease,
			Name:       tag,
			RepoPrefix: repoPrefix,
			Meta: map[string]any{
				"tag":        tag,
				"file_count": tagFileCount[tag],
				"order":      i, // 0 = oldest, len-1 = newest
			},
		}
		g.AddNode(node)
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

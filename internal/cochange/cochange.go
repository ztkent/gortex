// Package cochange mines git history for files that change together
// and projects the result onto the graph as EdgeCoChange edges.
//
// Co-change ("logical coupling") is the relationship git history
// reveals but the import graph cannot: a handler and its test, a
// struct and the serializer that mirrors it, a schema and its
// migration. Two files repeatedly committed together are coupled even
// when neither imports the other.
//
// Design mirrors internal/blame: shell out to `git log` (universally
// available, stable porcelain), best-effort (any git error yields an
// empty result rather than failing), file-level granularity (git
// tracks files, not symbols).
package cochange

import (
	"context"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Tuning defaults for a history scan.
const (
	DefaultMaxCommits        = 1500
	DefaultMaxFilesPerCommit = 40
	DefaultMinCount          = 2
)

// Options tunes the history scan. The zero value is valid — every
// field falls back to its Default* constant.
type Options struct {
	// MaxCommits caps how far back the scan walks.
	MaxCommits int
	// MaxFilesPerCommit drops a commit from pair counting when it
	// touches more files than this. Sweeping refactors, generated-file
	// regens, and vendor bumps couple everything with everything and
	// only add noise.
	MaxFilesPerCommit int
	// MinCount drops file pairs that co-changed fewer than this many
	// times — a single shared commit is not evidence of coupling.
	MinCount int
}

func (o Options) withDefaults() Options {
	if o.MaxCommits <= 0 {
		o.MaxCommits = DefaultMaxCommits
	}
	if o.MaxFilesPerCommit <= 0 {
		o.MaxFilesPerCommit = DefaultMaxFilesPerCommit
	}
	if o.MinCount <= 0 {
		o.MinCount = DefaultMinCount
	}
	return o
}

// Pair is one co-change relationship between two files. FileA and
// FileB are repo-relative paths, ordered lexically so the pair has a
// canonical form.
type Pair struct {
	FileA string  `json:"file_a"`
	FileB string  `json:"file_b"`
	Count int     `json:"count"` // commits touching both files
	Score float64 `json:"score"` // association strength, 0..1
}

// Result bundles a single Mine pass.
type Result struct {
	// Pairs is sorted by Score descending, then by file path.
	Pairs []Pair
	// FileChanges is the per-file commit-touch count over the scan.
	FileChanges map[string]int
	// CommitsScanned counts non-empty commits the scan walked.
	CommitsScanned int
}

// Mine runs `git log` in root and computes file-level co-change
// associations. Returns an empty Result (never an error) when root is
// not a git repository or git is unavailable — co-change is a
// best-effort enrichment.
func Mine(ctx context.Context, root string, opts Options) Result {
	opts = opts.withDefaults()
	empty := Result{FileChanges: map[string]int{}}
	if root == "" {
		return empty
	}
	// %x00 emits a NUL before each commit record; --name-only appends
	// the changed file list. The format body is empty — we only need
	// the file lists, grouped per commit by the NUL separator.
	cmd := exec.CommandContext(ctx, "git", "-C", root, "log", "--no-merges", //nolint:gosec // root is daemon-internal
		"-n", strconv.Itoa(opts.MaxCommits), "--name-only", "--format=%x00")
	out, err := cmd.Output()
	if err != nil {
		return empty
	}

	pairCounts := map[[2]string]int{}
	fileChanges := map[string]int{}
	commitsScanned := 0

	for _, chunk := range strings.Split(string(out), "\x00") {
		files := parseCommitFiles(chunk)
		if len(files) == 0 {
			continue
		}
		commitsScanned++
		// Per-file totals count every commit touching the file —
		// including solo and sweeping commits — so the association
		// denominator honestly reflects how often the file moves.
		for _, f := range files {
			fileChanges[f]++
		}
		// Pair counting skips solo commits (no pair) and sweeping
		// commits (noise). The remaining commits are genuine
		// co-change evidence.
		if len(files) < 2 || len(files) > opts.MaxFilesPerCommit {
			continue
		}
		for i := 0; i < len(files); i++ {
			for j := i + 1; j < len(files); j++ {
				pairCounts[orderedPair(files[i], files[j])]++
			}
		}
	}

	pairs := make([]Pair, 0, len(pairCounts))
	for key, cnt := range pairCounts {
		if cnt < opts.MinCount {
			continue
		}
		a, b := key[0], key[1]
		// Cosine association: count / sqrt(touchesA * touchesB).
		// Symmetric, in (0,1]; reaches 1 only when the two files
		// never move apart.
		score := 0.0
		if ca, cb := fileChanges[a], fileChanges[b]; ca > 0 && cb > 0 {
			score = float64(cnt) / math.Sqrt(float64(ca)*float64(cb))
		}
		if score > 1 {
			score = 1
		}
		pairs = append(pairs, Pair{FileA: a, FileB: b, Count: cnt, Score: score})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Score != pairs[j].Score {
			return pairs[i].Score > pairs[j].Score
		}
		if pairs[i].FileA != pairs[j].FileA {
			return pairs[i].FileA < pairs[j].FileA
		}
		return pairs[i].FileB < pairs[j].FileB
	})

	return Result{Pairs: pairs, FileChanges: fileChanges, CommitsScanned: commitsScanned}
}

// parseCommitFiles extracts the changed-file list from one NUL-
// delimited `git log --name-only` chunk. Every non-empty line in the
// chunk is a file path (the commit format body is empty).
func parseCommitFiles(chunk string) []string {
	chunk = strings.Trim(chunk, "\n")
	if chunk == "" {
		return nil
	}
	var files []string
	for _, line := range strings.Split(chunk, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files
}

// orderedPair returns the two paths as a lexically-ordered array so a
// pair has one canonical map key regardless of commit order.
func orderedPair(a, b string) [2]string {
	if a <= b {
		return [2]string{a, b}
	}
	return [2]string{b, a}
}

// EnrichGraph mines co-change for the repository rooted at root and
// adds symmetric EdgeCoChange edges between the matching KindFile
// nodes. Returns the number of directed edges added.
//
// repoPrefix scopes the file-node match in multi-repo graphs: when
// set, only file nodes carrying that RepoPrefix are considered and
// the git-relative paths are matched against the prefix-stripped node
// path. Pass "" for a single-repo graph.
//
// Best-effort: returns (0, nil) when root is not a git repository.
// Idempotent — graph.AddEdge dedupes, so repeated runs converge.
func EnrichGraph(g graph.Store, root, repoPrefix string) (int, error) {
	return EnrichGraphWith(g, root, repoPrefix, Options{})
}

// EnrichGraphWith is EnrichGraph with explicit scan tuning.
func EnrichGraphWith(g graph.Store, root, repoPrefix string, opts Options) (int, error) {
	if g == nil || root == "" {
		return 0, nil
	}
	res := Mine(context.Background(), root, opts)
	return AddEdges(g, res.Pairs, repoPrefix), nil
}

// AddEdges projects already-mined co-change pairs onto the graph as
// symmetric EdgeCoChange edges between the matching KindFile nodes,
// returning the number of directed edges added.
//
// repoPrefix scopes the file-node match: when set, only file nodes
// carrying that RepoPrefix are matched, against the prefix-stripped
// node path (the pairs hold git-relative paths). Pass "" for a
// single-repo graph. Idempotent — graph.AddEdge dedupes.
func AddEdges(g graph.Store, pairs []Pair, repoPrefix string) int {
	if g == nil || len(pairs) == 0 {
		return 0
	}
	// Map repo-relative path -> file node ID. A file node's FilePath
	// carries the multi-repo prefix; key on the prefix-stripped path
	// so a git-relative path matches.
	idByPath := make(map[string]string)
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindFile || n.FilePath == "" {
			continue
		}
		if repoPrefix != "" {
			if n.RepoPrefix != repoPrefix {
				continue
			}
			idByPath[strings.TrimPrefix(n.FilePath, repoPrefix+"/")] = n.ID
		} else {
			idByPath[n.FilePath] = n.ID
		}
	}

	added := 0
	for _, p := range pairs {
		idA, okA := idByPath[p.FileA]
		idB, okB := idByPath[p.FileB]
		if !okA || !okB || idA == idB {
			continue
		}
		g.AddEdge(coChangeEdge(idA, idB, p.FileA, p))
		g.AddEdge(coChangeEdge(idB, idA, p.FileB, p))
		added += 2
	}
	return added
}

// coChangeEdge builds one directed EdgeCoChange edge. Meta is cloned
// per edge so the two symmetric copies don't share a map.
func coChangeEdge(from, to, filePath string, p Pair) *graph.Edge {
	return &graph.Edge{
		From:       from,
		To:         to,
		Kind:       graph.EdgeCoChange,
		FilePath:   filePath,
		Origin:     graph.OriginASTInferred,
		Confidence: p.Score,
		Meta: map[string]any{
			"count": p.Count,
			"score": p.Score,
		},
	}
}

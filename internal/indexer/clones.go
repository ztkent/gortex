package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// cloneSigMetaKey is the Node.Meta key under which a function/method's
// base64-encoded MinHash signature is stored. The graph-wide LSH pass
// reads it back out — keeping the signature on the node makes the pass
// a pure graph walk (no file IO), correct under incremental reindex,
// and safe across multi-repo graphs.
const cloneSigMetaKey = "clone_sig"

// applyCloneSignatures is the per-file half of clone detection. It runs
// inside applyCoverageDomains (gated on the "clones" coverage domain),
// slices each function/method body out of the file source, computes a
// MinHash signature, and stamps it on the node's Meta. Bodies below
// clones.MinTokens normalised tokens produce no signature and are
// silently skipped — they are dominated by boilerplate and would only
// add noise to the LSH buckets.
//
// Allocation note: the body slicing path computes one []int of line
// offsets per file and one string per emitted body. The previous
// implementation went through splitLines (which materialises the
// whole source as N per-line Go strings) and a quadratic concat in
// bodyText (each iteration grew the output via "out += ..."). Profile
// showed bodyText + splitLinesUpTo at 3+ GiB per 30 s window — both
// are now O(file_bytes) one-shot allocations.
func applyCloneSignatures(src []byte, result *parser.ExtractionResult) {
	if result == nil || len(result.Nodes) == 0 {
		return
	}
	// Compute newline offsets once per file rather than splitting the
	// source into N Go strings. offsets[i] is the byte index where
	// line i+1 (1-indexed) starts; the sentinel offsets[len(offsets)-1]
	// is len(src) so the slice math doesn't need a special case for
	// the last line.
	offsets := lineOffsets(src)
	for _, n := range result.Nodes {
		if n == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		body := bodyTextFromOffsets(src, offsets, n.StartLine, n.EndLine)
		if body == "" {
			continue
		}
		sig, ok := clones.ComputeSignature(body)
		if !ok {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta[cloneSigMetaKey] = clones.EncodeSignature(sig)
	}
}

// lineOffsets returns the byte offsets of each line in src. For a file
// with N lines the result has length N+1: the first entry is 0, each
// subsequent entry is the byte index immediately after a '\n', and the
// final sentinel is len(src) so callers can slice the last line as
// src[offsets[N-1]:offsets[N]] without special-casing EOF.
//
// One allocation (the []int) instead of N (one string per line via
// strings.Split). Lifetime is per-file: the caller drops the slice
// when the file's worker batch finishes.
func lineOffsets(src []byte) []int {
	// Reserve a generous initial capacity to avoid repeated slice
	// growth on typical source files (~ 200 lines). The slice grows
	// from here for larger files; small files waste a bit of headroom
	// that goes back to the GC immediately.
	offsets := make([]int, 1, 256)
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			offsets = append(offsets, i+1)
		}
	}
	offsets = append(offsets, len(src))
	return offsets
}

// bodyTextFromOffsets returns src[startLine..endLine] (both 1-indexed,
// inclusive) as one Go string. The trailing newline of the last
// included line is stripped so output matches the old line-join
// semantics ("a\nb" not "a\nb\n"). Returns "" for degenerate or
// out-of-bounds ranges, matching bodyText.
func bodyTextFromOffsets(src []byte, offsets []int, startLine, endLine int) string {
	if startLine <= 0 || endLine < startLine {
		return ""
	}
	lo := startLine - 1
	hi := endLine
	// len(offsets) = lineCount + 1 (sentinel). lineCount = len(offsets) - 1.
	lineCount := len(offsets) - 1
	if lo >= lineCount {
		return ""
	}
	if hi > lineCount {
		hi = lineCount
	}
	startOff := offsets[lo]
	endOff := offsets[hi]
	// Strip the trailing '\n' that bounds the last included line so the
	// output matches the line-join semantics callers and tests expect.
	if endOff > startOff && endOff <= len(src) && endOff-1 >= 0 && src[endOff-1] == '\n' {
		endOff--
	}
	return string(src[startOff:endOff])
}

// bodyText returns the source spanning [startLine, endLine] (both
// 1-indexed, inclusive) joined by newlines. Kept as a legacy helper
// for the unit-test surface; production callers go through
// applyCloneSignatures → bodyTextFromOffsets, which avoids both the
// whole-source string copy in splitLines and the O(N²) concat below.
func bodyText(lines []string, startLine, endLine int) string {
	if startLine <= 0 || endLine < startLine {
		return ""
	}
	lo := startLine - 1
	hi := endLine
	if lo >= len(lines) {
		return ""
	}
	if hi > len(lines) {
		hi = len(lines)
	}
	// Precompute the joined size so the strings.Builder grows once,
	// turning the previous O(N²) "out += ..." into O(total_bytes).
	total := 0
	for i := lo; i < hi; i++ {
		total += len(lines[i])
		if i > lo {
			total++ // separating '\n'
		}
	}
	var b strings.Builder
	b.Grow(total)
	for i := lo; i < hi; i++ {
		if i > lo {
			b.WriteByte('\n')
		}
		b.WriteString(lines[i])
	}
	return b.String()
}

// CloneDetectionStats summarises one detectClonesAndEmitEdges run for
// the caller's logger. Exposed so the orchestrator can surface what the
// per-bucket cap dropped — a high skippedBucketItems means the
// workspace has a lot of templated boilerplate that LSH would have
// over-fanned-out on.
type CloneDetectionStats struct {
	Items                 int // function/method nodes with a signature
	Pairs                 int // detected clone pairs (after Jaccard filter)
	Edges                 int // EdgeSimilarTo emitted (≈ 2·Pairs, modulo dedup)
	SkippedBuckets        int // LSH buckets dropped for exceeding maxBucketSize
	SkippedBucketItems    int // total items inside the dropped buckets
}

// detectClonesAndEmitEdges is the graph-wide half of clone detection.
// It collects every function/method node carrying a clone_sig, runs
// the MinHash + LSH pass over their signatures, and materialises a
// symmetric pair of EdgeSimilarTo edges for each detected clone pair.
//
// threshold is the Jaccard similarity cutoff; pass 0 to use the
// clones package default. Returns clone stats including the per-bucket
// cap telemetry — the orchestrator logs that so a high skip count is
// visible during warmup.
//
// The pass is a full recompute and is idempotent: graph.AddEdge dedupes
// by edgeKey so re-emitting an unchanged pair is a no-op, and stale
// edges cannot survive — when either endpoint's file is reindexed,
// EvictFile removes that node's edges in both directions before this
// pass re-runs.
func detectClonesAndEmitEdges(g *graph.Graph, threshold float64) CloneDetectionStats {
	var stats CloneDetectionStats
	if g == nil {
		return stats
	}
	// Serialise against other graph-wide passes that mutate Node.Meta
	// (markTestSymbolsAndEmitEdges, ResolveTemporalCalls, reach.BuildIndex,
	// releases enrichment). Without this lock, the AllNodes walk below
	// reads n.Meta while one of those writers mutates the same map and
	// the runtime aborts with "concurrent map read and map write" — the
	// observed daemon crash. Shares g.ResolveMutex() so all such passes
	// rendezvous on the same lock the resolver already uses.
	g.ResolveMutex().Lock()
	defer g.ResolveMutex().Unlock()
	var items []clones.Item
	for _, n := range g.AllNodes() {
		if n == nil || n.Meta == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		enc, ok := n.Meta[cloneSigMetaKey].(string)
		if !ok || enc == "" {
			continue
		}
		sig, ok := clones.DecodeSignature(enc)
		if !ok {
			continue
		}
		items = append(items, clones.Item{ID: n.ID, Sig: sig})
	}
	stats.Items = len(items)
	if len(items) < 2 {
		return stats
	}

	detected, sb, sbi := clones.DetectPairsWithStats(items, threshold)
	stats.SkippedBuckets = sb
	stats.SkippedBucketItems = sbi
	stats.Pairs = len(detected)
	for _, p := range detected {
		from := g.GetNode(p.A)
		to := g.GetNode(p.B)
		if from == nil || to == nil {
			continue
		}
		emitSimilarEdge(g, from, to, p.Similarity)
		emitSimilarEdge(g, to, from, p.Similarity)
		stats.Edges += 2
	}
	return stats
}

// emitSimilarEdge adds one directed EdgeSimilarTo edge carrying the
// estimated Jaccard similarity. The edge is anchored at the source
// node's file/line for locality. Origin is ast_inferred — the
// relationship is a statistical estimate over normalised tokens, not a
// structural fact.
func emitSimilarEdge(g *graph.Graph, from, to *graph.Node, similarity float64) {
	g.AddEdge(&graph.Edge{
		From:       from.ID,
		To:         to.ID,
		Kind:       graph.EdgeSimilarTo,
		FilePath:   from.FilePath,
		Line:       from.StartLine,
		Confidence: similarity,
		Origin:     graph.OriginASTInferred,
		Meta:       map[string]any{"similarity": similarity},
	})
}

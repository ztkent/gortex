package rerank

// FileCoherenceSignal boosts a candidate when several other
// candidates in the same batch come from the same file. The intuition:
// when BM25 (or vector search) returns multiple hits from one file,
// that file is more likely the user's actual target — the file's
// signal is stronger than any single chunk's. Orthogonal to FanIn
// (which scores graph-topology centrality) because a single
// load-bearing symbol can still live in a file with no other hits.
//
// Algorithm: the per-file score is the sum of (1 / (text_rank + 1))
// across that file's candidates — lower rank = bigger contribution.
// Each candidate's contribution to this signal is its file's score
// divided by the largest per-file score in the batch, clamped to
// [0, 1]. Files with only one candidate contribute 0 to the signal
// (no "multi-chunk" evidence yet); the BM25 / semantic signals already
// reward solo-hit relevance, so the signal stays orthogonal.
//
// Concretely:
//   - File F has 3 candidates at ranks 0, 2, 9 → score = 1 + 1/3 + 1/10 = 1.43
//   - File G has 1 candidate at rank 1 → solo, signal contributes 0
//   - File H has 2 candidates at ranks 4, 7 → score = 1/5 + 1/8 = 0.325
//   - max = 1.43; F candidates contribute 1.0, H candidates contribute 0.23
type FileCoherenceSignal struct{}

// Name returns the canonical signal name registered in DefaultWeights.
func (FileCoherenceSignal) Name() string { return SignalFileCoherence }

// Contribute returns the per-candidate file-coherence boost in [0, 1].
// Returns 0 when the candidate's file isn't a multi-chunk file (≤1
// candidate from that file) or when the candidate has no usable
// file path / text rank.
func (FileCoherenceSignal) Contribute(_ string, c *Candidate, ctx *Context) float64 {
	if c == nil || c.Node == nil || ctx == nil {
		return 0
	}
	fp := c.Node.FilePath
	if fp == "" {
		return 0
	}
	group := ctx.fileGroups[fp]
	if len(group) < 2 {
		// Solo-hit file → no multi-chunk evidence. The BM25 / semantic
		// signals already cover this case.
		return 0
	}
	if ctx.maxFileScoreSum <= 0 {
		return 0
	}
	score := ctx.fileScoreSum[fp]
	if score <= 0 {
		return 0
	}
	out := score / ctx.maxFileScoreSum
	if out > 1 {
		out = 1
	}
	return out
}

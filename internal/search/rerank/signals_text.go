package rerank

// BM25Signal scores a candidate by its BM25 (text-search) rank. Rank
// 0 → 1.0, monotonically decreasing; absent (TextRank < 0) → 0. Uses
// the RRF reciprocal-rank kernel (k=60), normalised so the top hit
// returns exactly 1.0.
type BM25Signal struct{}

func (BM25Signal) Name() string { return SignalBM25 }

func (BM25Signal) Contribute(query string, c *Candidate, _ *Context) float64 {
	if c.TextRank < 0 {
		return 0
	}
	const k = 60.0
	const topNorm = 1.0 / (k + 1.0)
	v := 1.0 / (k + float64(c.TextRank) + 1.0)
	return v / topNorm
}

// SemanticSignal scores a candidate by its vector-search rank. Same
// kernel as BM25Signal.
type SemanticSignal struct{}

func (SemanticSignal) Name() string { return SignalSemantic }

func (SemanticSignal) Contribute(query string, c *Candidate, _ *Context) float64 {
	if c.VectorRank < 0 {
		return 0
	}
	const k = 60.0
	const topNorm = 1.0 / (k + 1.0)
	v := 1.0 / (k + float64(c.VectorRank) + 1.0)
	return v / topNorm
}

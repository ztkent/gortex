package rerank

import (
	"math"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Context bundles the read-only data signals need at scoring time.
// All fields are optional; signals must gracefully degrade when a
// data source is absent. The zero value is a valid Context.
type Context struct {
	// Graph is the indexed knowledge graph reader. Required for any
	// signal that reads node metadata or walks edges (FanIn, FanOut,
	// MinHash). When nil, those signals contribute 0. Held as the
	// `graph.Reader` interface so the editor-overlay path can pass
	// an `*OverlaidView` here and have rerank signals score against
	// the overlay's shadow graph just like base.
	Graph graph.Reader

	// QueryClass is the detected shape of the query (symbol / concept
	// / path / signature). It scales the bm25 and semantic signal
	// weights inside Pipeline.Rerank. The zero value QueryClassUnknown
	// tells Rerank to auto-detect via ClassifyQuery; a caller — the
	// search_symbols query_class argument — may pin it instead.
	QueryClass QueryClass

	// CommunityOf maps a node ID to its detected community ID. When
	// nil, the community signal contributes 0.
	CommunityOf func(nodeID string) string

	// RepoPrefix and ProjectID name the session's home repo and
	// project. Used by the community signal to score candidates by
	// locality. Both empty disables the locality side of the signal.
	RepoPrefix string
	ProjectID  string

	// ChurnOf returns a modification-count proxy. When non-nil the
	// churn signal uses it (typical source: MCP symbol history). When
	// nil the churn signal falls back to Node.Meta["churn"] or, if
	// absent, the count of distinct authors in
	// Node.Meta["last_authored"]. Returning 0 means "no churn data".
	ChurnOf func(nodeID string) int

	// FeedbackOf returns a per-symbol "useful to past tasks" score in
	// [-1, 1] (the same shape as feedbackManager.GetSymbolScore).
	// When nil the feedback component sits at 0.
	FeedbackOf func(nodeID string) float64

	// FrecencyBoostOf returns a frecency multiplier in
	// [1, maxFrecencyBoost] (the same shape as frecencyTracker.BoostFor).
	// 1.0 means "no boost". When nil it's treated as 1.0 everywhere.
	FrecencyBoostOf func(nodeID string) float64

	// ComboBoostOf returns a (query, symbol) co-occurrence multiplier
	// in [1, comboMaxBoost]. 1.0 means "no boost". When nil it's
	// treated as 1.0 everywhere.
	ComboBoostOf func(nodeID string) float64

	// Now provides the current unix time in seconds. Overridable for
	// tests; zero means "use time.Now().Unix()".
	Now int64

	// --- Internal scratch space populated by prepare(). ---

	// communityCount maps community ID → number of candidates in that
	// community. Used by the community signal to detect topic clusters.
	communityCount map[string]int
	// maxCommunityCount is the largest value in communityCount.
	maxCommunityCount int

	// fanInMax / fanOutMax cache the maximum fan counts across the
	// current candidate set so the log-normalised contributions stay
	// in [0,1].
	fanInMax  int
	fanOutMax int

	// churnMax caches max churn across the candidate set.
	churnMax int

	// candidateIDs is the set of node IDs in the current batch.
	// MinHash uses it to only count similarity edges that point to
	// other candidates in the same batch (cluster-cohesion signal).
	candidateIDs map[string]struct{}

	// fileGroups maps each file path → candidates from that file in
	// batch order. The file-coherence signal reads this to detect
	// "many candidates share this file" multi-chunk evidence and
	// boost the lead candidate from each file. Files with a single
	// candidate are present but contribute zero to the signal.
	fileGroups map[string][]*Candidate
	// fileScoreSum maps file path → sum of BM25-rank weights for the
	// candidates from that file (lower text rank = higher weight).
	// Drives the per-file evidence score; the multi-chunk signal
	// boosts the per-file lead by `fileScoreSum / maxFileScoreSum`.
	fileScoreSum map[string]float64
	// maxFileScoreSum is the largest value in fileScoreSum across
	// the batch; used to normalise the boost into [0, 1]. Zero when
	// no candidate has a usable text rank.
	maxFileScoreSum float64

	// pathPenaltyCache memoises the path-penalty multiplier per file
	// path within a single Rerank call so the regex-heavy rubric
	// runs once per file rather than once per candidate. Bounded by
	// the candidate set's file count.
	pathPenaltyCache map[string]float64
}

// now returns the active timestamp (test-injectable when Now != 0).
func (c *Context) now() int64 {
	if c.Now != 0 {
		return c.Now
	}
	return time.Now().Unix()
}

// prepare populates the internal scratch fields once per Rerank call.
// Idempotent — safe to call again after mutating the candidate slice.
func (c *Context) prepare(cands []*Candidate) {
	c.communityCount = make(map[string]int, len(cands))
	c.maxCommunityCount = 0
	c.candidateIDs = make(map[string]struct{}, len(cands))
	c.fanInMax = 0
	c.fanOutMax = 0
	c.churnMax = 0
	c.fileGroups = make(map[string][]*Candidate, len(cands))
	c.fileScoreSum = make(map[string]float64, len(cands))
	c.maxFileScoreSum = 0
	c.pathPenaltyCache = make(map[string]float64, len(cands))

	for _, cand := range cands {
		if cand == nil || cand.Node == nil {
			continue
		}
		c.candidateIDs[cand.Node.ID] = struct{}{}

		if c.CommunityOf != nil {
			com := c.CommunityOf(cand.Node.ID)
			if com != "" {
				c.communityCount[com]++
				if c.communityCount[com] > c.maxCommunityCount {
					c.maxCommunityCount = c.communityCount[com]
				}
			}
		}

		if c.Graph != nil {
			fi := len(c.Graph.GetInEdges(cand.Node.ID))
			fo := len(c.Graph.GetOutEdges(cand.Node.ID))
			if fi > c.fanInMax {
				c.fanInMax = fi
			}
			if fo > c.fanOutMax {
				c.fanOutMax = fo
			}
		}

		ch := c.churnFor(cand.Node)
		if ch > c.churnMax {
			c.churnMax = ch
		}

		// File grouping: collect candidates by FilePath and sum their
		// inverse-rank weights so the file-coherence signal can detect
		// multi-chunk evidence + identify the per-file lead candidate.
		fp := cand.Node.FilePath
		if fp != "" {
			c.fileGroups[fp] = append(c.fileGroups[fp], cand)
			if cand.TextRank >= 0 {
				w := 1.0 / float64(cand.TextRank+1)
				c.fileScoreSum[fp] += w
				if c.fileScoreSum[fp] > c.maxFileScoreSum {
					c.maxFileScoreSum = c.fileScoreSum[fp]
				}
			}
		}
	}
}

// churnFor consults the ChurnOf hook, then Node.Meta["churn"], then
// the distinct-author proxy. Returns 0 when no source has data.
func (c *Context) churnFor(n *graph.Node) int {
	if n == nil {
		return 0
	}
	if c.ChurnOf != nil {
		if v := c.ChurnOf(n.ID); v > 0 {
			return v
		}
	}
	if n.Meta == nil {
		return 0
	}
	switch v := n.Meta["churn"].(type) {
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	}
	// Fall back: distinct-author count if blame enrichment ran on
	// multiple commits. last_authored stores only the latest, so the
	// best we can do without a richer enrich pass is treat
	// authors_count when present, else 1 when at least one author
	// stamp exists, else 0.
	if v, ok := n.Meta["authors_count"]; ok {
		switch x := v.(type) {
		case int:
			if x > 0 {
				return x
			}
		case int64:
			if x > 0 {
				return int(x)
			}
		case float64:
			if x > 0 {
				return int(x)
			}
		}
	}
	if _, ok := n.Meta["last_authored"]; ok {
		return 1
	}
	return 0
}

// lastAuthoredUnix extracts the timestamp from Node.Meta["last_authored"].
// Returns 0 when absent or malformed.
func lastAuthoredUnix(n *graph.Node) int64 {
	if n == nil || n.Meta == nil {
		return 0
	}
	raw, ok := n.Meta["last_authored"]
	if !ok {
		return 0
	}
	switch v := raw.(type) {
	case map[string]any:
		switch ts := v["timestamp"].(type) {
		case int:
			return int64(ts)
		case int64:
			return ts
		case float64:
			return int64(ts)
		}
	}
	return 0
}

// normLog returns log(1+value) / log(1+max), clamped to [0, 1]. The
// log scale keeps a single dominant outlier from drowning the rest of
// the candidate set.
func normLog(value, max int) float64 {
	if value <= 0 || max <= 0 {
		return 0
	}
	out := math.Log1p(float64(value)) / math.Log1p(float64(max))
	if out < 0 {
		return 0
	}
	if out > 1 {
		return 1
	}
	return out
}

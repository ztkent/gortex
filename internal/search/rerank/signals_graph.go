package rerank

import "github.com/zzet/gortex/internal/graph"

// FanInSignal scores by log-normalised in-edge count. Symbols with
// many callers / references are more likely to be load-bearing and
// thus more likely to be what an agent is actually after.
type FanInSignal struct{}

func (FanInSignal) Name() string { return SignalFanIn }

func (FanInSignal) Contribute(_ string, c *Candidate, ctx *Context) float64 {
	if ctx.Graph == nil {
		return 0
	}
	count := len(ctx.Graph.GetInEdges(c.Node.ID))
	return normLog(count, ctx.fanInMax)
}

// FanOutSignal scores by log-normalised out-edge count. Pure utility
// functions tend to have low fan-out; coordinator / dispatcher code
// has high fan-out. A weak signal, but discriminating for navigation
// queries that target orchestration code.
type FanOutSignal struct{}

func (FanOutSignal) Name() string { return SignalFanOut }

func (FanOutSignal) Contribute(_ string, c *Candidate, ctx *Context) float64 {
	if ctx.Graph == nil {
		return 0
	}
	count := len(ctx.Graph.GetOutEdges(c.Node.ID))
	return normLog(count, ctx.fanOutMax)
}

// MinHashSignal scores by similarity-cluster cohesion: how many other
// candidates in the same result batch this candidate is MinHash-near
// to. Higher = the candidate sits at the center of a cluster of
// similar code, lower = isolated. Depends on the F9 clone-detection
// pass having stamped EdgeSimilarTo edges.
type MinHashSignal struct{}

func (MinHashSignal) Name() string { return SignalMinHash }

func (MinHashSignal) Contribute(_ string, c *Candidate, ctx *Context) float64 {
	if ctx.Graph == nil || len(ctx.candidateIDs) <= 1 {
		return 0
	}
	var total, n float64
	for _, e := range ctx.Graph.GetOutEdges(c.Node.ID) {
		if e.Kind != graph.EdgeSimilarTo {
			continue
		}
		if _, ok := ctx.candidateIDs[e.To]; !ok {
			continue
		}
		sim := similarityFromEdge(e)
		if sim <= 0 {
			continue
		}
		total += sim
		n++
	}
	// Symmetric edge — also walk incoming (snapshots that omit
	// outgoing copies of similar_to don't lose recall).
	for _, e := range ctx.Graph.GetInEdges(c.Node.ID) {
		if e.Kind != graph.EdgeSimilarTo {
			continue
		}
		if _, ok := ctx.candidateIDs[e.From]; !ok {
			continue
		}
		sim := similarityFromEdge(e)
		if sim <= 0 {
			continue
		}
		total += sim
		n++
	}
	if n == 0 {
		return 0
	}
	avg := total / n
	// Normalise by the cap of EdgeSimilarTo similarity (Jaccard ≤ 1).
	if avg > 1 {
		avg = 1
	}
	return avg
}

// similarityFromEdge mirrors the lookup used by tools_clones — prefer
// Meta["similarity"], fall back to Confidence so edges restored from
// older snapshots still contribute.
func similarityFromEdge(e *graph.Edge) float64 {
	if e == nil {
		return 0
	}
	if e.Meta != nil {
		if v, ok := e.Meta["similarity"].(float64); ok {
			return v
		}
	}
	return e.Confidence
}

// CommunitySignal scores by topic-cluster cohesion plus session
// locality. The contribution is the max of:
//
//   - same graph community as other top candidates (cluster cohesion)
//   - same repo as the session's home repo
//   - same project as the session's home project
//
// All three saturate at 1.0; the max collapses them so a candidate
// that's both same-repo and same-cluster doesn't double-count.
type CommunitySignal struct{}

func (CommunitySignal) Name() string { return SignalCommunity }

func (CommunitySignal) Contribute(_ string, c *Candidate, ctx *Context) float64 {
	var best float64

	if ctx.CommunityOf != nil && ctx.maxCommunityCount > 1 {
		com := ctx.CommunityOf(c.Node.ID)
		if com != "" {
			cnt := ctx.communityCount[com]
			if cnt > 1 {
				score := float64(cnt) / float64(ctx.maxCommunityCount)
				if score > best {
					best = score
				}
			}
		}
	}

	if ctx.RepoPrefix != "" && c.Node.RepoPrefix == ctx.RepoPrefix {
		if 1.0 > best {
			best = 1.0
		}
	} else if ctx.ProjectID != "" {
		proj := c.Node.ProjectID
		if proj == "" {
			proj = c.Node.RepoPrefix
		}
		if proj == ctx.ProjectID && 0.7 > best {
			best = 0.7
		}
	}

	return best
}

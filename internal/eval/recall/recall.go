// Package recall computes retrieval recall@K, latency, and token-return
// metrics for the Gortex retrieval stack against a curated fixture of
// {query, expected_ids} pairs.
//
// Methodology
//
// Recall is reported as any-hit set-level recall: a retrieval counts as
// correct at rank K if *any* of the Expected IDs for a case appears in
// the ranker's top-K results. Multiple Expected IDs per case are OK —
// they represent valid alternative targets (e.g. a type and its
// constructor both being reasonable answers to "BM25 backend").
//
// Cases are tiered so per-tier weakness is visible:
//
//   - exact:     symbol-name queries. Tests the basic "can you find a
//     named symbol I already know about" case. BM25 should
//     dominate here; a retrieval tool that can't ace exact
//     tier is broken.
//   - concept:   natural-language paraphrase queries. Tests semantic
//     understanding. This is where BM25 starts losing to
//     semantic / RRF.
//   - multi_hop: relational queries accepting several valid expected
//     IDs (any-hit). Tests graph-aware retrieval.
//
// Gold vs judged
//
// All fixture labels in this package are hand-curated gold labels, not
// LLM-judged. This is a stricter measurement than a dual-judge setup
// (CQS style) — the numbers you get here are lower than what a
// per-query LLM judge would award, because paraphrased-but-still-correct
// ranker output is scored as a miss unless it matches the gold ID.
//
// Used by the `gortex eval recall` CLI verb (roadmap I6).
package recall

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Tier classifies a case by query difficulty.
type Tier string

const (
	TierExact    Tier = "exact"
	TierConcept  Tier = "concept"
	TierMultiHop Tier = "multi_hop"
)

// Ranker names a retrieval strategy used in the output report.
type Ranker struct {
	Name string
	// Search returns a ranked list of symbol IDs for the query.
	Search func(query string, limit int) []string
}

// Case is a single fixture entry.
type Case struct {
	ID       string   `yaml:"id" json:"id,omitempty"`
	Tier     Tier     `yaml:"tier" json:"tier"`
	Query    string   `yaml:"query" json:"query"`
	Expected []string `yaml:"expected" json:"expected"`
	// WinnowConstraints is passed through to winnow-style rankers so a
	// case can pre-filter the candidate pool (e.g. {"language": "go"}).
	WinnowConstraints map[string]any `yaml:"winnow_constraints,omitempty" json:"winnow_constraints,omitempty"`
}

// Fixture is a collection of cases plus metadata.
type Fixture struct {
	Name  string `yaml:"name" json:"name"`
	Cases []Case `yaml:"cases" json:"cases"`
}

// Ks lists the rank cutoffs the report computes.
var Ks = []int{1, 5, 20}

// TokenCounter returns the tokenized length of an arbitrary string.
// Typically wired to tokens.Count (tiktoken cl100k_base) so the number
// agents actually pay is reported.
type TokenCounter func(string) int

// defaultTokenCounter falls back to a rough chars/4 estimate when no
// real counter is wired.
func defaultTokenCounter(s string) int { return (len(s) + 3) / 4 }

// RankerResult holds aggregated metrics for a single ranker over a
// fixture run. All maps are keyed by k or tier.
type RankerResult struct {
	Name       string               `json:"name"`
	Cases      int                  `json:"cases"`
	Hits       map[int]int          `json:"hits"`
	Recall     map[int]float64      `json:"recall"`
	MeanRRank  float64              `json:"mean_reciprocal_rank"`
	PerTier    map[Tier]TierMetrics `json:"per_tier,omitempty"`
	P50Micros  int64                `json:"p50_micros"`
	P95Micros  int64                `json:"p95_micros"`
	MaxMicros  int64                `json:"max_micros"`
	MeanTokens float64              `json:"mean_tokens_returned"`
	// Misses records cases where none of Expected appeared in top-K
	// at the biggest K in Ks. Populated for diagnostics / judge mode.
	Misses []Miss `json:"misses,omitempty"`
	// Note: skipped is true when the ranker was registered but had no
	// usable backend (e.g. semantic ranker with no embedder). Zero-
	// filled results help keep the report table shape stable.
	Skipped string `json:"skipped,omitempty"`
}

// Miss is a single per-case diagnostic row for when a ranker didn't
// surface any expected ID in top-K.
type Miss struct {
	CaseID   string   `json:"case_id"`
	Query    string   `json:"query"`
	Expected []string `json:"expected"`
	Top      []string `json:"top"` // top-K IDs the ranker returned
	// JudgedHit is populated by a post-hoc LLM-judge pass: true when
	// the judge accepts any entry in Top as answering the Query. Nil
	// pointer means "not judged."
	JudgedHit *bool `json:"judged_hit,omitempty"`
}

// TierMetrics is a per-tier recall slice.
type TierMetrics struct {
	Cases  int             `json:"cases"`
	Hits   map[int]int     `json:"hits"`
	Recall map[int]float64 `json:"recall"`
}

// Report bundles the results of a fixture run across every ranker.
type Report struct {
	Fixture   string         `json:"fixture"`
	Cases     int            `json:"cases"`
	PerTier   map[Tier]int   `json:"per_tier_cases"`
	Rankers   []RankerResult `json:"rankers"`
	GortexRev string         `json:"gortex_rev,omitempty"`
}

// Run evaluates every ranker against every case in the fixture and
// returns an aggregated report. Limits retrieval to the largest K in Ks.
func Run(fixture Fixture, rankers []Ranker, tc TokenCounter) Report {
	if tc == nil {
		tc = defaultTokenCounter
	}

	maxK := 0
	for _, k := range Ks {
		if k > maxK {
			maxK = k
		}
	}

	report := Report{
		Fixture: fixture.Name,
		Cases:   len(fixture.Cases),
		PerTier: make(map[Tier]int),
	}
	for _, c := range fixture.Cases {
		report.PerTier[c.Tier]++
	}

	for _, r := range rankers {
		report.Rankers = append(report.Rankers, evalOne(fixture, r, tc, maxK))
	}
	return report
}

// evalOne runs a single ranker across the fixture and aggregates.
func evalOne(fixture Fixture, r Ranker, tc TokenCounter, maxK int) RankerResult {
	res := RankerResult{
		Name:    r.Name,
		Cases:   len(fixture.Cases),
		Hits:    make(map[int]int, len(Ks)),
		Recall:  make(map[int]float64, len(Ks)),
		PerTier: make(map[Tier]TierMetrics),
	}
	// Initialise per-tier buckets so the JSON shape is stable. The stock
	// tiers are always included for schema consistency; any additional
	// tier seen in the fixture (e.g. "di") is added on the fly so the
	// harness doesn't panic when fixtures grow new categories.
	for _, tier := range []Tier{TierExact, TierConcept, TierMultiHop} {
		res.PerTier[tier] = TierMetrics{
			Hits:   make(map[int]int, len(Ks)),
			Recall: make(map[int]float64, len(Ks)),
		}
	}
	for _, c := range fixture.Cases {
		if _, ok := res.PerTier[c.Tier]; c.Tier != "" && !ok {
			res.PerTier[c.Tier] = TierMetrics{
				Hits:   make(map[int]int, len(Ks)),
				Recall: make(map[int]float64, len(Ks)),
			}
		}
	}

	var rrSum, tokensSum float64
	latencies := make([]time.Duration, 0, len(fixture.Cases))

	for _, c := range fixture.Cases {
		expected := stringSet(c.Expected)

		start := time.Now()
		ranked := r.Search(c.Query, maxK)
		elapsed := time.Since(start)
		latencies = append(latencies, elapsed)

		// Token-return metric: tokenize the ranked ID list as a proxy
		// for "how many tokens does this ranker's top-K cost to return?"
		// Gives a rough Pareto knob (recall vs cost).
		var payload string
		for _, id := range ranked {
			payload += id + "\n"
		}
		tokensSum += float64(tc(payload))

		firstHit := -1
		for i, id := range ranked {
			if expected[id] {
				firstHit = i
				break
			}
		}
		for _, k := range Ks {
			if firstHit >= 0 && firstHit < k {
				res.Hits[k]++
			}
		}
		if firstHit >= 0 {
			rrSum += 1.0 / float64(firstHit+1)
		} else {
			// Capture a miss row: top-K returned + gold expected. Lets
			// --verbose print actionable diagnostics and lets --judge
			// rescue the case.
			topCopy := append([]string(nil), ranked...)
			expCopy := append([]string(nil), c.Expected...)
			res.Misses = append(res.Misses, Miss{
				CaseID:   c.ID,
				Query:    c.Query,
				Expected: expCopy,
				Top:      topCopy,
			})
		}

		// Per-tier bookkeeping.
		tier := c.Tier
		if tier == "" {
			tier = TierExact
		}
		tm := res.PerTier[tier]
		tm.Cases++
		for _, k := range Ks {
			if firstHit >= 0 && firstHit < k {
				tm.Hits[k]++
			}
		}
		res.PerTier[tier] = tm
	}

	if res.Cases > 0 {
		for _, k := range Ks {
			res.Recall[k] = float64(res.Hits[k]) / float64(res.Cases)
		}
		res.MeanRRank = rrSum / float64(res.Cases)
		res.MeanTokens = tokensSum / float64(res.Cases)
	}
	for tier, tm := range res.PerTier {
		if tm.Cases > 0 {
			for _, k := range Ks {
				tm.Recall[k] = float64(tm.Hits[k]) / float64(tm.Cases)
			}
		}
		res.PerTier[tier] = tm
	}

	res.P50Micros, res.P95Micros, res.MaxMicros = latencyPercentiles(latencies)
	return res
}

// latencyPercentiles sorts a copy and returns p50/p95/max in microseconds.
func latencyPercentiles(lats []time.Duration) (int64, int64, int64) {
	if len(lats) == 0 {
		return 0, 0, 0
	}
	sorted := append([]time.Duration(nil), lats...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p := func(pct float64) time.Duration {
		idx := int(float64(len(sorted)-1) * pct)
		return sorted[idx]
	}
	toMicros := func(d time.Duration) int64 { return d.Microseconds() }
	return toMicros(p(0.50)), toMicros(p(0.95)), toMicros(sorted[len(sorted)-1])
}

// stringSet turns a slice into a membership map.
func stringSet(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

// Markdown renders the report as a Markdown document. Stable ordering so
// the output is diffable across runs.
func Markdown(report Report) string {
	var b strings.Builder
	b.WriteString("# Gortex retrieval recall\n\n")
	if report.GortexRev != "" {
		fmt.Fprintf(&b, "_Fixture: `%s` · rev: `%s` · %d cases_\n\n",
			report.Fixture, report.GortexRev, report.Cases)
	} else {
		fmt.Fprintf(&b, "_Fixture: `%s` · %d cases_\n\n",
			report.Fixture, report.Cases)
	}
	if len(report.PerTier) > 0 {
		tierKeys := make([]Tier, 0, len(report.PerTier))
		for t := range report.PerTier {
			tierKeys = append(tierKeys, t)
		}
		sort.Slice(tierKeys, func(i, j int) bool { return tierKeys[i] < tierKeys[j] })
		b.WriteString("Tier distribution:")
		for _, t := range tierKeys {
			fmt.Fprintf(&b, " `%s`=%d", t, report.PerTier[t])
		}
		b.WriteString("\n\n")
	}

	b.WriteString("## Overall\n\n")
	b.WriteString("| ranker | R@1 | R@5 | R@20 | MRR | p50 µs | p95 µs | tokens/q |\n")
	b.WriteString("|--------|-----|-----|------|-----|--------|--------|----------|\n")

	rankers := append([]RankerResult(nil), report.Rankers...)
	sort.Slice(rankers, func(i, j int) bool { return rankers[i].Name < rankers[j].Name })
	for _, r := range rankers {
		if r.Skipped != "" {
			fmt.Fprintf(&b, "| %s | — | — | — | — | — | — | — (skipped: %s) |\n", r.Name, r.Skipped)
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %.3f | %d | %d | %.0f |\n",
			r.Name,
			pct(r.Recall[1]),
			pct(r.Recall[5]),
			pct(r.Recall[20]),
			r.MeanRRank,
			r.P50Micros,
			r.P95Micros,
			r.MeanTokens,
		)
	}

	b.WriteString("\n## Per tier (R@5)\n\n")
	b.WriteString("| ranker | exact | concept | multi_hop |\n")
	b.WriteString("|--------|-------|---------|-----------|\n")
	for _, r := range rankers {
		if r.Skipped != "" {
			fmt.Fprintf(&b, "| %s | — | — | — |\n", r.Name)
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			r.Name,
			pct(r.PerTier[TierExact].Recall[5]),
			pct(r.PerTier[TierConcept].Recall[5]),
			pct(r.PerTier[TierMultiHop].Recall[5]),
		)
	}

	return b.String()
}

func pct(f float64) string { return fmt.Sprintf("%5.1f%%", f*100) }


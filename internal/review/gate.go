package review

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/config"
)

// GateStats is the structured suppression summary the Gate returns alongside the
// kept findings. Each counter records how many findings a single drop reason
// removed, so a caller can report "N suppressed (below confidence / below
// severity / out of category / over cap)" without re-deriving the arithmetic.
type GateStats struct {
	// Input is the number of findings the gate was handed.
	Input int `json:"input"`
	// Kept is the number that survived every filter and the cap.
	Kept int `json:"kept"`
	// BelowConfidence counts findings dropped because their Confidence was
	// under the configured MinConfidence.
	BelowConfidence int `json:"below_confidence"`
	// BelowSeverity counts findings dropped because their Severity ranked
	// below the configured MinSeverity.
	BelowSeverity int `json:"below_severity"`
	// CategoryFiltered counts findings dropped because their Category was not
	// in the configured allow-list.
	CategoryFiltered int `json:"category_filtered"`
	// OverCap counts findings trimmed because the kept set exceeded
	// MaxFindings — the lowest-ranked findings are the ones dropped.
	OverCap int `json:"over_cap"`
}

// Suppressed is the total number of findings the gate removed for any reason.
func (g GateStats) Suppressed() int {
	return g.BelowConfidence + g.BelowSeverity + g.CategoryFiltered + g.OverCap
}

// Gate is a deterministic confidence / severity / category / cap filter over a
// finding set, driven by config.ReviewConfig. A zero-value Gate (zero-value
// config) is a pass-through: it keeps every finding and reports only Input /
// Kept. Construct one with NewGate, or zero-value it inline for the no-op case.
type Gate struct {
	minConfidence float64
	minSeverity   Severity
	categories    map[string]bool
	maxFindings   int
}

// NewGate compiles a Gate from the review config. An empty Categories list
// means "all categories"; an empty / unrecognised MinSeverity means "no
// severity floor" (info, the lowest rank); a zero MinConfidence keeps every
// confidence; a zero (or negative) MaxFindings means "no cap". The whole
// zero-value config therefore yields a pass-through gate.
func NewGate(cfg config.ReviewConfig) Gate {
	g := Gate{
		minConfidence: cfg.MinConfidence,
		maxFindings:   cfg.MaxFindings,
	}
	if s := normalizeSeverity(cfg.MinSeverity); cfg.MinSeverity != "" {
		g.minSeverity = s
	}
	if len(cfg.Categories) > 0 {
		g.categories = make(map[string]bool, len(cfg.Categories))
		for _, c := range cfg.Categories {
			c = normalizeCategory(c)
			if c != "" {
				g.categories[c] = true
			}
		}
	}
	return g
}

// Apply filters the findings and returns the kept set plus a structured
// suppression summary. The pipeline runs in order: drop below MinConfidence →
// drop below MinSeverity → drop out-of-category → stable-sort worst-first
// (severity desc, then confidence desc) → cap at MaxFindings (keeping the
// worst). The input slice is never mutated; the kept slice is a fresh
// allocation. A zero-value Gate keeps everything.
func (g Gate) Apply(findings []Finding) ([]Finding, GateStats) {
	stats := GateStats{Input: len(findings)}

	kept := make([]Finding, 0, len(findings))
	for _, f := range findings {
		// Confidence floor. A MinConfidence of 0 admits every finding (a
		// zero-confidence finding passes a zero floor).
		if g.minConfidence > 0 && f.Confidence < g.minConfidence {
			stats.BelowConfidence++
			continue
		}
		// Severity floor. An empty MinSeverity leaves minSeverity at its
		// zero value (SevInfo's rank, the lowest), so nothing is dropped.
		if severityRank(f.Severity) < severityRank(g.minSeverity) {
			stats.BelowSeverity++
			continue
		}
		// Category allow-list. A nil map means "all categories".
		if g.categories != nil && !g.categories[normalizeCategory(f.Category)] {
			stats.CategoryFiltered++
			continue
		}
		kept = append(kept, f)
	}

	// Rank worst-first so the cap trims the least-severe / least-confident
	// findings. Stable so equal-rank findings keep their input order.
	sort.SliceStable(kept, func(i, j int) bool {
		si, sj := severityRank(kept[i].Severity), severityRank(kept[j].Severity)
		if si != sj {
			return si > sj
		}
		return kept[i].Confidence > kept[j].Confidence
	})

	if g.maxFindings > 0 && len(kept) > g.maxFindings {
		stats.OverCap = len(kept) - g.maxFindings
		kept = kept[:g.maxFindings]
	}

	stats.Kept = len(kept)
	return kept, stats
}

// normalizeCategory lower-cases and trims a category so the allow-list match is
// case / whitespace insensitive — the same tolerance dedupeFindings applies.
func normalizeCategory(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

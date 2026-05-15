package rerank

// FeedbackSignal collapses three session-side sources into one
// contribution:
//
//   - feedbackManager.GetSymbolScore (post-task user/agent feedback,
//     [-1, 1])
//   - frecencyTracker.BoostFor (recent-access exponential decay,
//     [1, maxFrecencyBoost], passed as a multiplier)
//   - comboManager.BoostMap (per-(query, symbol) co-occurrence
//     boost, [1, comboMaxBoost], passed as a multiplier)
//
// Each component is normalised to [0, 1] before being merged with
// max(); merging by max (not sum) avoids triple-counting when a
// symbol is hot on all three axes.
type FeedbackSignal struct{}

func (FeedbackSignal) Name() string { return SignalFeedback }

func (FeedbackSignal) Contribute(_ string, c *Candidate, ctx *Context) float64 {
	var best float64

	if ctx.FeedbackOf != nil {
		// FeedbackOf returns [-1, 1]; map to [0, 1] with negatives
		// clamped — a "not needed" history shouldn't bury a strong
		// BM25 hit, just lose its bonus.
		raw := ctx.FeedbackOf(c.Node.ID)
		if raw > 0 {
			if raw > 1 {
				raw = 1
			}
			if raw > best {
				best = raw
			}
		}
	}

	if ctx.FrecencyBoostOf != nil {
		// Boost is in [1, maxFrecencyBoost=1.5]. Normalise to [0, 1]
		// over that range so a saturated boost matches an
		// always-useful feedback signal.
		raw := (ctx.FrecencyBoostOf(c.Node.ID) - 1.0) / 0.5
		if raw > 0 {
			if raw > 1 {
				raw = 1
			}
			if raw > best {
				best = raw
			}
		}
	}

	if ctx.ComboBoostOf != nil {
		// Boost is in [1, comboMaxBoost=3.0]. Normalise to [0, 1]
		// over that range.
		raw := (ctx.ComboBoostOf(c.Node.ID) - 1.0) / 2.0
		if raw > 0 {
			if raw > 1 {
				raw = 1
			}
			if raw > best {
				best = raw
			}
		}
	}

	return best
}

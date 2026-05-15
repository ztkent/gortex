package rerank

import (
	"math"

	"github.com/zzet/gortex/internal/graph"
)

// ChurnSignal scores by recent modification activity. Hot files
// (frequently touched) are more likely to be the agent's current
// concern. Reads from Context.churnFor which checks ChurnOf, then
// Node.Meta["churn"], then a 0/1 fallback derived from blame.
type ChurnSignal struct{}

func (ChurnSignal) Name() string { return SignalChurn }

func (ChurnSignal) Contribute(_ string, c *Candidate, ctx *Context) float64 {
	count := ctx.churnFor(c.Node)
	return normLog(count, ctx.churnMax)
}

// RecencySignal scores by recency of the last commit that touched the
// symbol. Reads Node.Meta["last_authored"].timestamp; symbols never
// authored (no blame enrichment) contribute 0. Exponential decay with
// a 30-day half-life so a year-old change still contributes ~6%.
type RecencySignal struct{}

func (RecencySignal) Name() string { return SignalRecency }

const recencyHalfLifeDays = 30.0
const recencySecondsPerDay = 86400.0

func (RecencySignal) Contribute(_ string, c *Candidate, ctx *Context) float64 {
	ts := lastAuthoredUnix(c.Node)
	if ts <= 0 {
		return 0
	}
	now := ctx.now()
	if now <= 0 {
		return 0
	}
	dt := float64(now-ts) / recencySecondsPerDay
	if dt < 0 {
		dt = 0
	}
	k := math.Ln2 / recencyHalfLifeDays
	return math.Exp(-k * dt)
}

// APISignatureSignal scores by query-vs-function-signature token
// overlap. Targets queries like "validate token return error" against
// `func validateToken(t string) error`. Asymmetric (overlap, not
// jaccard) so a verbose signature isn't penalised for extra tokens.
type APISignatureSignal struct{}

func (APISignatureSignal) Name() string { return SignalAPISignature }

func (APISignatureSignal) Contribute(query string, c *Candidate, _ *Context) float64 {
	if c.Node.Kind != graph.KindFunction && c.Node.Kind != graph.KindMethod {
		return 0
	}
	sig := signatureString(c.Node)
	if sig == "" {
		return 0
	}
	q := tokenize(query)
	if len(q) == 0 {
		return 0
	}
	target := tokenize(sig)
	// Stitch the symbol name into the target — the function name is
	// the API surface and should count even if the signature only
	// carries parameter types.
	target = append(target, tokenize(c.Node.Name)...)
	return overlap(q, target)
}

// TypeSignatureSignal scores by query-vs-type-signature overlap on
// type / interface / struct-field-bearing nodes. Targets queries like
// "user record id email" against `type User struct { ID; Email }`.
type TypeSignatureSignal struct{}

func (TypeSignatureSignal) Name() string { return SignalTypeSignature }

func (TypeSignatureSignal) Contribute(query string, c *Candidate, _ *Context) float64 {
	switch c.Node.Kind {
	case graph.KindType, graph.KindInterface, graph.KindEnumMember, graph.KindField:
		// In scope.
	default:
		return 0
	}
	q := tokenize(query)
	if len(q) == 0 {
		return 0
	}
	target := tokenize(c.Node.Name)
	if sig := signatureString(c.Node); sig != "" {
		target = append(target, tokenize(sig)...)
	}
	// Pull in qualified name tokens (path/Type.Field) too — useful
	// for nested-field queries.
	if c.Node.QualName != "" {
		target = append(target, tokenize(c.Node.QualName)...)
	}
	return overlap(q, target)
}

// signatureString extracts the textual signature stored in Node.Meta
// when the extractor captured one. Different language extractors stamp
// different keys; we check the common ones in priority order so callers
// don't need to know the language.
func signatureString(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	for _, key := range []string{"signature", "sig", "fn_signature", "type_signature"} {
		if v, ok := n.Meta[key]; ok {
			switch s := v.(type) {
			case string:
				if s != "" {
					return s
				}
			}
		}
	}
	return ""
}

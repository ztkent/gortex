package astquery

// Idiomatic / correctness review rulepack. These detectors register
// exactly like the SAST / hygiene rules — through mustRegisterSAST —
// but carry Category = CategoryReview so the analyze layer can fan
// them out independently of the security set.
//
// Two classes of rule live here:
//
//   - Decidable rules (nil-deref-prone type assertions, inverted
//     error checks) fire on a self-contained structural shape and need
//     no further context. They surface as-is.
//
//   - Undecidable-from-AST-alone rules (check-then-act on a map, a
//     query call inside a loop body that smells of N+1) are emitted
//     optimistically here and then refined by the graph-grounding
//     post-pass in the review layer, where the resolved call / loop
//     metadata is reachable. The detectors stay pure-AST: their
//     PostFilters only ever see (parser.QueryResult, []byte).
//
// All rules cover Go and Python.

func init() {
	registerGoReviewRules()
	registerPyReviewRules()
}

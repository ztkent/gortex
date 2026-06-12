package astquery

// Python idiomatic / correctness review detectors. Severity
// error|warning; Category CategoryReview. The N+1 and check-then-act
// rules are emitted optimistically and refined by the graph-grounding
// post-pass.

func registerPyReviewRules() {
	mustRegisterSAST(
		// --- NPE-class footgun: a mutable default argument. The list /
		// dict is created once at def-time and shared across every
		// call, so mutating it leaks state between invocations. Use
		// `None` and build the container in the body.
		sastRule{
			Name:        "py-mutable-default-arg",
			Description: "Mutable default argument (`def f(x=[])` / `def f(x={})`) — the default is created once and shared across all calls, leaking state between invocations. Default to `None` and build the container inside the function.",
			Severity:    "warning",
			Category:    CategoryReview,
			Tags:        []string{"npe", "logic-error", "correctness"},
			Pat: map[string]string{
				"python": `((default_parameter value: [(list) (dictionary) (set)] @default) @match)`,
			},
		},

		// --- Logic error: a comparison of an identifier with itself,
		// which is constant (always True for ==, always False for !=).
		// Almost always a typo for a different operand.
		sastRule{
			Name:        "py-self-comparison",
			Description: "Comparison of an expression with itself (`if x == x:`) is constant — always True (or always False for `!=`). Almost certainly a typo; one side should reference a different value.",
			Severity:    "error",
			Category:    CategoryReview,
			Tags:        []string{"logic-error", "correctness"},
			Pat: map[string]string{
				"python": `((comparison_operator
                              . (identifier) @lhs
                              . ["==" "!="]
                              . (identifier) @rhs .) @match
                            (#eq? @lhs @rhs))`,
			},
		},

		// --- Check-then-act (TOCTOU / thread-safety): a `not in`
		// membership check on a dict immediately followed by a write to
		// the same dict. Emitted optimistically; the grounding
		// post-pass keeps it only when the enclosing scope shows a
		// mutating shape.
		sastRule{
			Name:        "py-check-then-act-dict",
			Description: "`if k not in d: d[k] = …` reads then writes the same dict without holding a lock across both — a check-then-act race when the dict is shared between threads / tasks. Use `setdefault`, a single locked region, or `dict.get` with a default.",
			Severity:    "warning",
			Category:    CategoryReview,
			Tags:        []string{"check-then-act", "toctou", "thread-safety", "concurrency"},
			Pat: map[string]string{
				"python": `((if_statement
                              condition: (comparison_operator
                                (identifier)
                                "not in"
                                (identifier) @coll)
                              consequence: (block
                                (expression_statement
                                  (assignment
                                    left: (subscript value: (identifier) @target))))) @match
                            (#eq? @coll @target))`,
			},
		},

		// --- N+1: a query-shaped call inside a for-loop body. Emitted
		// optimistically; the grounding post-pass drops it when the
		// enclosing symbol provably contains no loop (loop_depth==0).
		sastRule{
			Name:        "py-loop-query-call",
			Description: "Database / query-shaped call inside a `for` loop body — a classic N+1 query. Batch the lookups into a single round-trip (one IN-query, a join, or a prefetch) instead of querying per iteration.",
			Severity:    "warning",
			Category:    CategoryReview,
			Tags:        []string{"n-plus-one", "performance", "correctness"},
			Pat: map[string]string{
				"python": `((for_statement
                              body: (block
                                (expression_statement
                                  (call
                                    function: (attribute
                                      attribute: (identifier) @fn))))) @match
                            (#match? @fn "^(execute|executemany|query|get|filter|fetch|fetchone|fetchall|find|find_one|first|all|scalar)$"))`,
			},
		},
	)
}

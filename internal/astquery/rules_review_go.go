package astquery

// Go idiomatic / correctness review detectors. Severity error|warning;
// Category CategoryReview. The N+1 and check-then-act rules are emitted
// optimistically and refined by the graph-grounding post-pass.

func registerGoReviewRules() {
	mustRegisterSAST(
		// --- NPE: single-result type assertion that panics on the
		// wrong dynamic type. The comma-ok form `v, ok := x.(T)` is
		// the safe idiom and does not match (it parses as an
		// expression_list with two children).
		sastRule{
			Name:        "go-unchecked-type-assertion",
			Description: "Single-result type assertion `x.(T)` panics when the dynamic type doesn't match. Use the comma-ok form `v, ok := x.(T)` and handle the failure.",
			Severity:    "warning",
			Category:    CategoryReview,
			Tags:        []string{"npe", "panic", "correctness"},
			Pat: map[string]string{
				"go": `((short_var_declaration
                          left: (expression_list . (identifier) .)
                          right: (expression_list (type_assertion_expression) @assert)) @match)`,
			},
		},

		// --- Logic error: an inverted error check that returns the
		// (nil) error it just confirmed is nil. Almost always a
		// flipped `!=` / `==`.
		sastRule{
			Name:        "go-inverted-err-check",
			Description: "`if err == nil { return err }` returns the error on the nil branch — an inverted check that silently swallows the real failure. The guard is almost certainly meant to be `err != nil`.",
			Severity:    "error",
			Category:    CategoryReview,
			Tags:        []string{"logic-error", "error-handling", "correctness"},
			Pat: map[string]string{
				"go": `((if_statement
                          condition: (binary_expression
                            left: (identifier) @errvar
                            operator: "=="
                            right: (nil))
                          consequence: (block
                            (statement_list
                              (return_statement
                                (expression_list (identifier) @retvar))))) @match
                        (#eq? @errvar @retvar)
                        (#match? @errvar "(?i)err"))`,
			},
		},

		// --- Check-then-act (TOCTOU / thread-safety): a map presence
		// check immediately followed by a write to the same map under
		// the !ok branch. Emitted optimistically; the grounding
		// post-pass keeps it only when the enclosing scope shows a
		// concurrent / mutating shape.
		sastRule{
			Name:        "go-check-then-act-map",
			Description: "`if _, ok := m[k]; !ok { m[k] = … }` reads then writes the same map without holding a lock across both — a check-then-act race when the map is shared. Hold the lock over the whole read-modify-write or use a single atomic operation.",
			Severity:    "warning",
			Category:    CategoryReview,
			Tags:        []string{"check-then-act", "toctou", "thread-safety", "concurrency"},
			Pat: map[string]string{
				"go": `((if_statement
                          initializer: (short_var_declaration
                            right: (expression_list (index_expression operand: (identifier) @check)))
                          condition: (unary_expression operand: (identifier))
                          consequence: (block
                            (statement_list
                              (assignment_statement
                                left: (expression_list (index_expression operand: (identifier) @act)))))) @match
                        (#eq? @check @act))`,
			},
		},

		// --- N+1: a query-shaped call inside a for-loop body. Emitted
		// optimistically; the grounding post-pass drops it when the
		// enclosing symbol provably contains no loop (loop_depth==0).
		sastRule{
			Name:        "go-loop-query-call",
			Description: "Database / query-shaped call inside a `for` loop body — a classic N+1 query. Batch the lookups into a single round-trip (one IN-query or a join) instead of querying per iteration.",
			Severity:    "warning",
			Category:    CategoryReview,
			Tags:        []string{"n-plus-one", "performance", "correctness"},
			Pat: map[string]string{
				"go": `((for_statement
                          body: (block
                            (statement_list
                              (expression_statement
                                (call_expression
                                  function: (selector_expression
                                    field: (field_identifier) @fn)))))) @match
                        (#match? @fn "^(Query|QueryRow|QueryContext|QueryRowContext|Exec|ExecContext|Get|Select|Find|First|Scan)$"))`,
			},
		},
	)
}

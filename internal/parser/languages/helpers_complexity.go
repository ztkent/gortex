package languages

import (
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// CyclomaticComplexity returns the McCabe cyclomatic complexity of a
// function body — 1 plus the number of decision points (branches that
// can take more than one path). The body is walked once recursively;
// nested function/class definitions are skipped because their
// complexity belongs to their own nodes, not to the enclosing scope.
//
// The decision-point set is the cross-language overlap of common
// branch nodes, plus per-language extensions. Each language passes
// its own table of node-type names; tree-sitter grammars vary
// (`if_statement` vs `if_expression`, etc.) so we don't hardcode a
// single set here.
//
// Returns 1 for an empty / nil body — the canonical "no branches"
// score.
func CyclomaticComplexity(body *sitter.Node, decisionTypes map[string]bool, skipDescent map[string]bool) int {
	score := 1
	if body == nil || len(decisionTypes) == 0 {
		return score
	}
	walkComplexity(body, decisionTypes, skipDescent, &score)
	return score
}

func walkComplexity(n *sitter.Node, decisionTypes, skipDescent map[string]bool, score *int) {
	if n == nil {
		return
	}
	t := n.Type()
	if decisionTypes[t] {
		*score++
	}
	if skipDescent != nil && skipDescent[t] {
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		walkComplexity(n.NamedChild(i), decisionTypes, skipDescent, score)
	}
}

// Cross-language decision-point tables. Each value is a map for O(1)
// lookup. Tree-sitter grammars vary on AST node names — Go uses
// `if_statement`, Rust uses `if_expression`, Python uses
// `if_statement`, etc. Each language's complexity counter passes the
// table that matches its grammar.
//
// Boolean operator nodes (`&&`/`||`/`and`/`or`) are intentionally NOT
// in these tables today. Counting them double-counts conditions and
// makes scores noisy on guards like `if a && b && c`. If a project
// wants strict McCabe parity later, add `binary_expression` plus a
// post-filter that checks the operator text.

var goComplexityNodes = map[string]bool{
	"if_statement":       true,
	"for_statement":      true,
	"expression_switch_statement": true,
	"type_switch_statement":       true,
	"select_statement":            true,
	"case_clause":                 true,
	"communication_case":          true,
	"type_case":                   true,
}

var goComplexitySkip = map[string]bool{
	"func_literal":         true, // closures
	"function_declaration": true, // nested defs (rare in Go)
	"method_declaration":   true,
}

var tsComplexityNodes = map[string]bool{
	"if_statement":          true,
	"for_statement":         true,
	"for_in_statement":      true,
	"for_of_statement":      true,
	"while_statement":       true,
	"do_statement":          true,
	"switch_case":           true,
	"switch_default":        true,
	"catch_clause":          true,
	"ternary_expression":    true,
	"conditional_expression": true,
}

var tsComplexitySkip = map[string]bool{
	"function_declaration": true,
	"function_expression":  true,
	"arrow_function":       true,
	"method_definition":    true,
	"class_declaration":    true,
}

var pyComplexityNodes = map[string]bool{
	"if_statement":          true,
	"elif_clause":           true,
	"for_statement":         true,
	"while_statement":       true,
	"except_clause":         true,
	"match_statement":       true,
	"case_clause":           true,
	"conditional_expression": true,
	"list_comprehension":    true,
	"dictionary_comprehension": true,
	"set_comprehension":     true,
	"generator_expression":  true,
}

var pyComplexitySkip = map[string]bool{
	"function_definition":  true,
	"class_definition":     true,
	"lambda":               true,
	"decorated_definition": true,
}

var rustComplexityNodes = map[string]bool{
	"if_expression":      true,
	"if_let_expression":  true,
	"for_expression":     true,
	"while_expression":   true,
	"loop_expression":    true,
	"match_arm":          true,
	"match_expression":   true,
}

var rustComplexitySkip = map[string]bool{
	"function_item":     true,
	"closure_expression": true,
}

var javaComplexityNodes = map[string]bool{
	"if_statement":         true,
	"for_statement":        true,
	"enhanced_for_statement": true,
	"while_statement":      true,
	"do_statement":         true,
	"switch_label":         true,
	"switch_block_statement_group": true,
	"catch_clause":         true,
	"ternary_expression":   true,
}

var javaComplexitySkip = map[string]bool{
	"method_declaration":      true,
	"constructor_declaration": true,
	"lambda_expression":       true,
	"class_declaration":       true,
}

// GoComplexity / TSComplexity / PyComplexity / RustComplexity /
// JavaComplexity — convenience wrappers picking the right table.
// Pass the function/method's body block (not the whole declaration)
// so the count excludes any header-side noise.
func GoComplexity(body *sitter.Node) int {
	return CyclomaticComplexity(body, goComplexityNodes, goComplexitySkip)
}

func TSComplexity(body *sitter.Node) int {
	return CyclomaticComplexity(body, tsComplexityNodes, tsComplexitySkip)
}

func PyComplexity(body *sitter.Node) int {
	return CyclomaticComplexity(body, pyComplexityNodes, pyComplexitySkip)
}

func RustComplexity(body *sitter.Node) int {
	return CyclomaticComplexity(body, rustComplexityNodes, rustComplexitySkip)
}

func JavaComplexity(body *sitter.Node) int {
	return CyclomaticComplexity(body, javaComplexityNodes, javaComplexitySkip)
}

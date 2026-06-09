package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// C++ signature metadata extraction. The overload resolver needs each
// function/method's parameter types, count, required count (minus defaults),
// variadic flag, and per-param shape (value / pointer / lvalue-ref / rvalue-ref
// + const) to rank candidates. These are stamped as compact Meta strings
// (gob/sqlite-roundtrip-safe) on the node:
//
//	cpp_param_types  = comma-joined normalized base types ("int,double")
//	cpp_param_shapes = parallel shape codes ("v,cp,l")  (see paramShapeCode)
//	cpp_req_params   = number of non-default parameters
//	cpp_variadic     = "1" when the signature ends in "..."
//
// param count is len(cpp_param_types). The base type is normalized so int vs
// long stays distinct (unlike GitNexus which collapses them) while cv/ref/ptr
// and namespace qualifiers are stripped for a stable comparison key.

// cppSignature is the extracted per-call-target signature.
type cppSignature struct {
	ParamTypes  []string
	ParamShapes []string
	ReqParams   int
	Variadic    bool
}

// extractCppSignature walks a function_definition (or declaration) node's
// parameter list. ok is false only when no function_declarator is reachable.
func extractCppSignature(funcNode *sitter.Node, src []byte) (sig cppSignature, ok bool) {
	if funcNode == nil {
		return sig, false
	}
	decl := findCppFunctionDeclarator(funcNode)
	if decl == nil {
		return sig, false
	}
	params := decl.ChildByFieldName("parameters")
	if params == nil {
		return sig, true // no parameter list visible — treat as zero params
	}
	// `...` may be an anonymous token rather than a named
	// variadic_parameter_declaration node depending on grammar version.
	if strings.Contains(params.Content(src), "...") {
		sig.Variadic = true
	}
	n := int(params.NamedChildCount())
	for i := 0; i < n; i++ {
		p := params.NamedChild(i)
		if p == nil {
			continue
		}
		switch p.Type() {
		case "variadic_parameter_declaration":
			sig.Variadic = true
		case "parameter_declaration", "optional_parameter_declaration":
			typeText := cppParamTypeText(p, src)
			// `(void)` is C++ for an explicit empty parameter list.
			if n == 1 && strings.TrimSpace(typeText) == "void" && p.ChildByFieldName("declarator") == nil {
				return sig, true
			}
			sig.ParamTypes = append(sig.ParamTypes, graph.NormalizeCppType(typeText))
			sig.ParamShapes = append(sig.ParamShapes, paramShapeCode(p, typeText, src))
			if p.Type() == "parameter_declaration" {
				sig.ReqParams++
			}
		}
	}
	return sig, true
}

// stampCppSignature writes the extracted signature onto a node's Meta. The
// cpp_sig marker lets the resolver tell "0 params, signature known" apart from
// "no signature extracted".
func stampCppSignature(meta map[string]any, funcNode *sitter.Node, src []byte) {
	sig, ok := extractCppSignature(funcNode, src)
	if !ok {
		return
	}
	meta["cpp_sig"] = "1"
	if len(sig.ParamTypes) > 0 {
		meta["cpp_param_types"] = strings.Join(sig.ParamTypes, ",")
		meta["cpp_param_shapes"] = strings.Join(sig.ParamShapes, ",")
	}
	if sig.ReqParams > 0 {
		meta["cpp_req_params"] = sig.ReqParams
	}
	if sig.Variadic {
		meta["cpp_variadic"] = "1"
	}
}

// findCppFunctionDeclarator descends through return-type pointer/reference
// wrappers to the function_declarator carrying the parameter list.
func findCppFunctionDeclarator(node *sitter.Node) *sitter.Node {
	cur := node.ChildByFieldName("declarator")
	for cur != nil {
		switch cur.Type() {
		case "function_declarator":
			return cur
		case "pointer_declarator", "reference_declarator", "parenthesized_declarator":
			cur = cur.ChildByFieldName("declarator")
		default:
			return nil
		}
	}
	return nil
}

func cppParamTypeText(p *sitter.Node, src []byte) string {
	if t := p.ChildByFieldName("type"); t != nil {
		return t.Content(src)
	}
	return ""
}

// paramShapeCode encodes a parameter's indirection + const-ness into one token:
// optional 'c' (const) followed by v(alue) / p(ointer) / l(value-ref) /
// r(value-ref). The const flag is read from the type field text.
func paramShapeCode(p *sitter.Node, typeText string, src []byte) string {
	prefix := ""
	// `const` is a sibling type_qualifier, not part of the type field, so check
	// the whole parameter spelling.
	if _ = typeText; strings.Contains(p.Content(src), "const") {
		prefix = "c"
	}
	d := p.ChildByFieldName("declarator")
	for d != nil {
		switch d.Type() {
		case "pointer_declarator", "abstract_pointer_declarator":
			return prefix + "p"
		case "reference_declarator", "abstract_reference_declarator":
			if strings.Contains(d.Content(src), "&&") {
				return prefix + "r"
			}
			return prefix + "l"
		case "parenthesized_declarator":
			d = d.ChildByFieldName("declarator")
		default:
			return prefix + "v"
		}
	}
	return prefix + "v"
}

// Type normalization lives in graph.NormalizeCppType so the resolver shares it.

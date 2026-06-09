package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// In-engine C++ overload resolution. Picks which same-named function/method a
// call binds to by ISO C++ rules — arity (with defaults + variadics), then
// implicit-conversion-sequence (ICS) ranking with pairwise dominance for the
// best-viable function — entirely from the signature metadata the cpp extractor
// stamps, no compiler needed. It runs in CI/sandbox where clangd cannot.
//
// Strict invariant (from GitNexus, sharpened): DEGRADE, NEVER LIE. Any axis it
// cannot decide keeps the candidate; a genuinely ambiguous best-viable set
// (≥2 non-dominated) returns nil so the resolver suppresses the edge rather
// than binding the wrong overload.

const cppRankInf = 1 << 30

// cppArithmetic is the set of normalized arithmetic base types eligible for
// standard arithmetic conversions (rank 2).
var cppArithmetic = map[string]bool{
	"int": true, "double": true, "char": true, "bool": true,
	"long": true, "short": true, "float": true, "unsigned": true,
}

// cppIntegralPromotion maps a small integral type to its promoted form
// (rank 1, better than a general arithmetic conversion).
var cppIntegralPromotion = map[string]string{
	"char": "int", "bool": "int", "short": "int",
}

// cppShape is the decoded per-parameter indirection sidecar.
type cppShape struct {
	isPointer bool
	isLRef    bool
	isRRef    bool
	isConst   bool
}

func decodeCppShape(code string) cppShape {
	s := cppShape{}
	if strings.HasPrefix(code, "c") {
		s.isConst = true
		code = code[1:]
	}
	switch code {
	case "p":
		s.isPointer = true
	case "l":
		s.isLRef = true
	case "r":
		s.isRRef = true
	}
	return s
}

// cppConversionRank returns the implicit-conversion-sequence rank from argType
// to paramType (lower = better): 0 exact, 1 integral promotion, 2 standard
// conversion (arithmetic, nullptr→T*, T*→bool, T*→void*), 3 nullptr→bool,
// 5 ellipsis, cppRankInf mismatch. (User-defined conversions — rank 4 — need a
// converting-ctor index not yet built; their absence means a UDC-only match is
// conservatively a non-match, never a wrong bind.)
func cppConversionRank(argType, paramType string, arg, param cppShape) int {
	if argType == paramType {
		if exactShapeCompatible(arg, param) {
			return 0
		}
		return cppRankInf
	}
	if paramType == "..." {
		return 5
	}
	if cppIntegralPromotion[argType] == paramType && paramType != "" {
		return 1
	}
	if cppArithmetic[argType] && cppArithmetic[paramType] {
		return 2
	}
	if argType == "null" && param.isPointer {
		return 2
	}
	if argType == "null" && paramType == "bool" {
		return 3
	}
	if arg.isPointer && paramType == "bool" {
		return 2
	}
	if arg.isPointer && param.isPointer && paramType == "void" {
		return 2
	}
	return cppRankInf
}

// exactShapeCompatible: an exact base-type match is only a rank-0 conversion
// when the indirection agrees (int ≠ int*). A value arg binds to a value or a
// (const) reference parameter; a pointer arg binds to a pointer parameter.
func exactShapeCompatible(a, p cppShape) bool {
	return a.isPointer == p.isPointer
}

// cppCandSig is a candidate's parsed signature.
type cppCandSig struct {
	node       *graph.Node
	paramTypes []string
	shapes     []cppShape
	reqParams  int
	variadic   bool
}

// parseCppCandidate reads the cpp_* signature Meta off a node. ok is false when
// the node carries no extracted signature (so the resolver can't rank it).
func parseCppCandidate(n *graph.Node) (cppCandSig, bool) {
	if n == nil || n.Meta == nil {
		return cppCandSig{}, false
	}
	if _, ok := n.Meta["cpp_sig"]; !ok {
		return cppCandSig{}, false
	}
	c := cppCandSig{node: n}
	if pt, _ := n.Meta["cpp_param_types"].(string); pt != "" {
		c.paramTypes = strings.Split(pt, ",")
	}
	if ps, _ := n.Meta["cpp_param_shapes"].(string); ps != "" {
		for _, code := range strings.Split(ps, ",") {
			c.shapes = append(c.shapes, decodeCppShape(code))
		}
	}
	c.reqParams = cppMetaInt(n.Meta, "cpp_req_params")
	if _, ok := n.Meta["cpp_variadic"]; ok {
		c.variadic = true
	}
	return c, true
}

func cppMetaInt(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// ResolveCppOverload selects the best-viable overload among same-name
// candidates, or nil to degrade to the caller's namespace cascade.
func ResolveCppOverload(argHints []string, candidates []*graph.Node) *graph.Node {
	var sigs []cppCandSig
	for _, c := range candidates {
		if c == nil || (c.Kind != graph.KindFunction && c.Kind != graph.KindMethod) {
			continue
		}
		s, ok := parseCppCandidate(c)
		if !ok {
			continue // no signature → not rankable; leave to the cascade
		}
		if !cppArityCompatible(s, len(argHints)) {
			continue
		}
		sigs = append(sigs, s)
	}
	switch len(sigs) {
	case 0:
		return nil
	case 1:
		return sigs[0].node
	}
	// Multiple arity-viable candidates: need argument types to rank further.
	if len(argHints) == 0 {
		return nil // can't disambiguate → suppress
	}
	normArgs := make([]string, len(argHints))
	for i, a := range argHints {
		normArgs[i] = graph.NormalizeCppType(a)
	}
	argShapes := make([]cppShape, len(argHints)) // literal/value args; unknown = value

	type ranked struct {
		node *graph.Node
		vec  []int
	}
	var viable []ranked
	for _, s := range sigs {
		vec := make([]int, len(normArgs))
		bad := false
		for j := range normArgs {
			if normArgs[j] == "" {
				// Unknown arg type: compatible with any parameter, and neutral
				// for dominance (every candidate scores 0 here). Degrade, never
				// lie — an untyped arg never makes a candidate non-viable.
				vec[j] = 0
				continue
			}
			pt, psh := cppParamAt(s, j)
			r := cppConversionRank(normArgs[j], pt, argShapes[j], psh)
			if r >= cppRankInf {
				bad = true
				break
			}
			vec[j] = r
		}
		if !bad {
			viable = append(viable, ranked{s.node, vec})
		}
	}
	switch len(viable) {
	case 0:
		return nil
	case 1:
		return viable[0].node
	}
	// Pairwise dominance → non-dominated set ([over.ics.rank]).
	var nondom []ranked
	for i := range viable {
		dominated := false
		for k := range viable {
			if i != k && cppDominates(viable[k].vec, viable[i].vec) {
				dominated = true
				break
			}
		}
		if !dominated {
			nondom = append(nondom, viable[i])
		}
	}
	if len(nondom) == 1 {
		return nondom[0].node
	}
	return nil // ≥2 non-dominated → ambiguous → suppress (never lie)
}

func cppParamAt(s cppCandSig, j int) (string, cppShape) {
	if j < len(s.paramTypes) {
		sh := cppShape{}
		if j < len(s.shapes) {
			sh = s.shapes[j]
		}
		return s.paramTypes[j], sh
	}
	if s.variadic {
		return "...", cppShape{}
	}
	return "", cppShape{}
}

func cppArityCompatible(s cppCandSig, argCount int) bool {
	if s.variadic {
		return argCount >= s.reqParams
	}
	return argCount >= s.reqParams && argCount <= len(s.paramTypes)
}

// cppDominates: a is not-worse-everywhere and strictly-better-somewhere than b.
func cppDominates(a, b []int) bool {
	better := false
	for i := range a {
		if a[i] > b[i] {
			return false
		}
		if a[i] < b[i] {
			better = true
		}
	}
	return better
}

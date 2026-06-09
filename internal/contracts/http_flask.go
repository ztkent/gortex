package contracts

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Non-decorator Flask routing forms. Gortex's per-line httpPatterns table can
// only see @app.route / @router.get decorators; the two imperative Flask forms
// below reference their handler off the call line, so they resolve it against
// the file's already-extracted symbol nodes:
//
//   - flask-restful: api.add_resource(ResourceClass, '/path'[, '/path2'])
//     maps the class's get/post/put/delete/... methods to HTTP verbs.
//   - imperative:    app.add_url_rule('/path', view_func=fn, methods=[...])
//     the procedural equivalent of @app.route.
//
// Both emit Contracts in the exact shape the decorator path produces, so the
// indexer materialises the same KindContract node + EdgeProvides +
// EdgeHandlesRoute with no further wiring. Unlike pygraph (which points every
// add_resource route at the class), gortex points each per-verb route at the
// specific method node so analyze kind=routes / get_callers land on the real
// method body.

var (
	addResourceCallRE = regexp.MustCompile(`\.add_resource\s*\(`)
	addURLRuleCallRE  = regexp.MustCompile(`\.add_url_rule\s*\(`)
	viewFuncKwargRE   = regexp.MustCompile(`view_func\s*=\s*([A-Za-z_][\w.]*)`)
	methodsKwargRE    = regexp.MustCompile(`methods\s*=\s*(\[[^\]]*\]|\([^)]*\)|["'][^"']*["'])`)
	quotedTokenRE     = regexp.MustCompile(`["']([^"']+)["']`)
	pyIdentRE         = regexp.MustCompile(`^[A-Za-z_][\w.]*$`)
)

// flaskVerbMethods is the closed set of resource methods flask-restful maps to
// HTTP verbs, in canonical order.
var flaskVerbMethods = []string{"get", "post", "put", "delete", "patch", "head", "options"}

// extractFlaskRestfulRoutes handles api.add_resource(ResourceClass, '/p'...).
func (h *HTTPExtractor) extractFlaskRestfulRoutes(filePath, text string, lines []string, fileNodes []*graph.Node, lang string, tree *parser.ParseTree) []Contract {
	var out []Contract
	for _, m := range addResourceCallRE.FindAllStringIndex(text, -1) {
		args := callTrailSlice(text, m[0])
		if args == "" {
			continue
		}
		parts := splitPyParams(args)
		if len(parts) < 2 {
			continue
		}
		className := pyBareIdentTail(strings.TrimSpace(parts[0]))
		if className == "" {
			continue
		}
		var paths []string
		for _, p := range parts[1:] {
			if lit, ok := pyStringLiteral(p); ok && lit != "" {
				paths = append(paths, lit)
			}
		}
		if len(paths) == 0 {
			continue
		}
		lineNum := lineAtOffset(lines, m[0])
		classID := filePath + "::" + className
		verbMethods := resourceVerbMethods(fileNodes, classID)
		classNode := findTypeNodeByName(fileNodes, className)

		for _, path := range paths {
			if len(verbMethods) > 0 {
				for _, verb := range flaskVerbMethods {
					mid, ok := verbMethods[verb]
					if !ok {
						continue
					}
					out = append(out, h.buildFlaskContract(filePath, strings.ToUpper(verb), path, mid, lineNum, lines, fileNodes, lang, tree))
				}
				continue
			}
			// GET fallback (parity with pygraph): the class is unresolvable or
			// defines no HTTP-verb methods, but a route node should still
			// appear for discoverability. Attach to the class node when known,
			// else the enclosing symbol.
			sym := findEnclosingSymbol(fileNodes, lineNum)
			if classNode != nil {
				sym = classNode.ID
			}
			out = append(out, h.buildFlaskContract(filePath, "GET", path, sym, lineNum, lines, fileNodes, lang, tree))
		}
	}
	return out
}

// extractFlaskAddURLRule handles app.add_url_rule('/p', view_func=fn, methods=[...]).
func (h *HTTPExtractor) extractFlaskAddURLRule(filePath, text string, lines []string, fileNodes []*graph.Node, lang string, tree *parser.ParseTree) []Contract {
	var out []Contract
	for _, m := range addURLRuleCallRE.FindAllStringIndex(text, -1) {
		args := callTrailSlice(text, m[0])
		if args == "" {
			continue
		}
		parts := splitPyParams(args)
		if len(parts) == 0 {
			continue
		}
		path, ok := pyStringLiteral(parts[0])
		if !ok || path == "" {
			continue
		}
		// view_func from kwarg, else positional arg[1] if it is a bare ident.
		viewFunc := ""
		if mm := viewFuncKwargRE.FindStringSubmatch(args); len(mm) == 2 {
			viewFunc = mm[1]
		} else if len(parts) >= 2 {
			cand := strings.TrimSpace(parts[1])
			if pyIdentRE.MatchString(cand) && !strings.Contains(cand, "=") {
				viewFunc = cand
			}
		}
		if viewFunc == "" {
			// Parity with pygraph: only emit when a view_func resolved.
			continue
		}
		viewFunc = pyBareIdentTail(viewFunc)

		methods := flaskMethodsKwarg(args)
		if len(methods) == 0 {
			methods = []string{"GET"}
		}
		lineNum := lineAtOffset(lines, m[0])
		symbolID := findFunctionByName(fileNodes, viewFunc)
		if symbolID == "" {
			symbolID = findEnclosingSymbol(fileNodes, lineNum)
		}
		for _, method := range methods {
			out = append(out, h.buildFlaskContract(filePath, method, path, symbolID, lineNum, lines, fileNodes, lang, tree))
		}
	}
	return out
}

// buildFlaskContract builds a provider HTTP contract identical in shape to the
// decorator path so the indexer materialises the same graph elements.
func (h *HTTPExtractor) buildFlaskContract(filePath, method, path, symbolID string, lineNum int, lines []string, fileNodes []*graph.Node, lang string, tree *parser.ParseTree) Contract {
	normPath, origNames := NormalizeHTTPPathWithParams(path)
	contractID := fmt.Sprintf("http::%s::%s", method, normPath)
	meta := map[string]any{
		"method":    method,
		"path":      normPath,
		"framework": "flask",
	}
	if len(origNames) > 0 {
		meta["path_param_names"] = origNames
	}
	c := Contract{
		ID:         contractID,
		Type:       ContractHTTP,
		Role:       RoleProvider,
		SymbolID:   symbolID,
		FilePath:   filePath,
		Line:       lineNum,
		Meta:       meta,
		Confidence: 0.9,
	}
	EnrichHTTPContractWithTree(&c, lines, fileNodes, lang, tree)
	return c
}

// resourceVerbMethods returns verb→methodNodeID for the HTTP-verb methods a
// resource class defines, keyed on the deterministic method node ID
// (filePath::Class.verb).
func resourceVerbMethods(fileNodes []*graph.Node, classID string) map[string]string {
	out := map[string]string{}
	for _, v := range flaskVerbMethods {
		want := classID + "." + v
		for _, n := range fileNodes {
			if n.Kind == graph.KindMethod && n.ID == want {
				out[v] = n.ID
				break
			}
		}
	}
	return out
}

// findTypeNodeByName resolves a class/type node by short name (findFunctionByName
// deliberately excludes KindType, so add_resource needs its own lookup).
func findTypeNodeByName(fileNodes []*graph.Node, name string) *graph.Node {
	for _, n := range fileNodes {
		if n.Kind == graph.KindType && n.Name == name {
			return n
		}
	}
	return nil
}

// flaskMethodsKwarg parses methods=['POST','PUT'] / methods=('GET',) /
// methods="GET" into upper-cased verb names.
func flaskMethodsKwarg(argsText string) []string {
	mm := methodsKwargRE.FindStringSubmatch(argsText)
	if len(mm) < 2 {
		return nil
	}
	var out []string
	for _, q := range quotedTokenRE.FindAllStringSubmatch(mm[1], -1) {
		out = append(out, strings.ToUpper(q[1]))
	}
	return out
}

// pyStringLiteral strips matching quotes (and an optional r/f/b prefix) from a
// Python string-literal token, reporting whether the token was a literal.
func pyStringLiteral(tok string) (string, bool) {
	tok = strings.TrimSpace(tok)
	for len(tok) > 1 {
		switch tok[0] {
		case 'r', 'R', 'f', 'F', 'b', 'B', 'u', 'U':
			if tok[1] == '\'' || tok[1] == '"' {
				tok = tok[1:]
				continue
			}
		}
		break
	}
	if len(tok) >= 2 {
		q := tok[0]
		if (q == '\'' || q == '"') && tok[len(tok)-1] == q {
			return tok[1 : len(tok)-1], true
		}
	}
	return "", false
}

// pyBareIdentTail strips a module qualifier (pkg.UserResource → UserResource)
// and returns "" if the result is not a bare identifier.
func pyBareIdentTail(tok string) string {
	tok = strings.TrimSpace(tok)
	if i := strings.LastIndex(tok, "."); i >= 0 {
		tok = tok[i+1:]
	}
	if tok == "" || !pyIdentRE.MatchString(tok) {
		return ""
	}
	return tok
}

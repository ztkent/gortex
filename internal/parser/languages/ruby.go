package languages

import (
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/ruby"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// qRubyAll is a single tree-sitter query alternating over every pattern
// the Ruby extractor needs. One tree walk per file replaces the 7
// `parser.RunQuery` calls the previous design made. Capture names are
// disjoint across patterns so the dispatch in Extract can branch on
// which name is set. Class/module membership for methods and singleton
// methods is resolved via a strict parent walk
// (method → body_statement → class) — nested defs inside another
// method's body fall through to the free-function bucket, mirroring
// the legacy nested-query semantics exactly.
const qRubyAll = `
[
  (class
    name: (constant) @class.name) @class.def

  (module
    name: (constant) @mod.name) @mod.def

  (method
    name: (identifier) @method.name) @method.def

  (singleton_method
    name: (identifier) @singleton.name) @singleton.def

  (call
    method: (identifier) @req.method
    arguments: (argument_list
      (string (string_content) @req.path))) @req.def

  (call
    method: (identifier) @call.name) @call.expr

  (assignment
    left: (constant) @const.name
    right: (_) @const.value) @const.def
]
`

// RubyExtractor extracts Ruby source files into graph nodes and edges.
type RubyExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewRubyExtractor() *RubyExtractor {
	lang := ruby.GetLanguage()
	return &RubyExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qRubyAll, lang),
	}
}

func (e *RubyExtractor) Language() string     { return "ruby" }
func (e *RubyExtractor) Extensions() []string { return []string{".rb", ".rake", ".gemspec"} }

// --- Deferred call buffer ----------------------------------------

type rubyDeferredCall struct {
	name    string
	line    int
	hasRecv bool
}

func (e *RubyExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID:        filePath,
		Kind:      graph.KindFile,
		Name:      filePath,
		FilePath:  filePath,
		StartLine: 1,
		EndLine:   int(root.EndPoint().Row) + 1,
		Language:  "ruby",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	var calls []rubyDeferredCall

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["class.def"] != nil:
			e.emitClass(m, filePath, fileID, result, seen)

		case m.Captures["mod.def"] != nil:
			e.emitModule(m, filePath, fileID, result, seen)

		case m.Captures["method.def"] != nil:
			e.emitMethod(m, filePath, fileID, src, result, seen)

		case m.Captures["singleton.def"] != nil:
			e.emitSingletonMethod(m, filePath, fileID, src, result, seen)

		case m.Captures["req.def"] != nil:
			e.emitRequire(m, filePath, fileID, result)

		case m.Captures["call.expr"] != nil:
			name := m.Captures["call.name"].Text
			if name == "require" || name == "require_relative" {
				// Handled by the require pattern above.
				return
			}
			expr := m.Captures["call.expr"]
			hasRecv := false
			if expr.Node != nil {
				if expr.Node.ChildByFieldName("receiver") != nil {
					hasRecv = true
				}
			}
			calls = append(calls, rubyDeferredCall{
				name:    name,
				line:    expr.StartLine + 1,
				hasRecv: hasRecv,
			})

		case m.Captures["const.def"] != nil:
			e.emitConstant(m, filePath, fileID, result, seen)
		}
	})

	// Resolve call edges against funcRanges.
	funcRanges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		target := "unresolved::" + c.name
		if c.hasRecv {
			target = "unresolved::*." + c.name
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: target,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		})
	}

	// Rails-style callback dispatch — preserves legacy behaviour exactly.
	emitRailsCallbacks(root, src, filePath, result)

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

func (e *RubyExtractor) emitClass(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["class.name"].Text
	def := m.Captures["class.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "ruby",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *RubyExtractor) emitModule(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["mod.name"].Text
	def := m.Captures["mod.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindPackage, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "ruby",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitMethod classifies a `def name` definition: direct child of a
// class's body_statement → method of that class; anything else → free
// top-level function. Mirrors the legacy qRbClassMethod +
// qRbMethod-fallback semantics.
func (e *RubyExtractor) emitMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["method.name"].Text
	def := m.Captures["method.def"]
	startLine1 := def.StartLine + 1

	className := rubyDirectClassParent(def.Node, src)
	if className != "" {
		id := filePath + "::" + className + "." + name
		if seen[id] {
			return
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
			Language: "ruby", Meta: map[string]any{
				"receiver":  className,
				"signature": "def " + name,
			},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
		})
		classID := filePath + "::" + className
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
		})
		return
	}

	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "ruby", Meta: map[string]any{"signature": "def " + name},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
}

// emitSingletonMethod mirrors the legacy qRbSingletonMethod pattern.
// `def self.foo` (and other `def receiver.foo` forms) is only
// meaningful as a class method — if we can't attribute it to an
// enclosing class, skip, matching legacy behaviour.
func (e *RubyExtractor) emitSingletonMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["singleton.name"].Text
	def := m.Captures["singleton.def"]
	startLine1 := def.StartLine + 1

	className := rubyDirectClassParent(def.Node, src)
	if className == "" {
		return
	}
	id := filePath + "::" + className + "." + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "ruby", Meta: map[string]any{
			"receiver":  className,
			"signature": "def " + name,
		},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	classID := filePath + "::" + className
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
	})
}

func (e *RubyExtractor) emitRequire(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	method := m.Captures["req.method"].Text
	if method != "require" && method != "require_relative" {
		return
	}
	path := m.Captures["req.path"]
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + path.Text,
		Kind: graph.EdgeImports, FilePath: filePath, Line: path.StartLine + 1,
	})
}

func (e *RubyExtractor) emitConstant(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["const.name"].Text
	def := m.Captures["const.def"]
	if len(name) == 0 || !isUpperASCII(name[0]) {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "ruby",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

// rubyDirectClassParent returns the enclosing class name when the
// method/singleton_method is a direct child of a class's body_statement
// (mirrors the legacy nested qRbClassMethod / qRbSingletonMethod
// patterns). Returns "" for nested-in-method definitions or top-level
// defs, preserving the legacy free-function bucket.
func rubyDirectClassParent(def *sitter.Node, src []byte) string {
	if def == nil {
		return ""
	}
	parent := def.Parent()
	if parent == nil || parent.Type() != "body_statement" {
		return ""
	}
	grand := parent.Parent()
	if grand == nil || grand.Type() != "class" {
		return ""
	}
	nameNode := grand.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	return nameNode.Content(src)
}

// railsCallbackMethods enumerates the Rails controller macros that
// bind callbacks to actions. `skip_*` is intentionally excluded —
// it removes an inherited binding, and correctly honouring it would
// require parent-class tracking that's out of scope for the first
// pass. The negative-space impact is small; the positive binding
// from the parent class still surfaces as an edge.
var railsCallbackMethods = map[string]struct{}{
	"before_action":  {},
	"after_action":   {},
	"around_action":  {},
	"before_filter":  {},
	"after_filter":   {},
	"around_filter":  {},
}

func emitRailsCallbacks(root *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	// Walk every class body looking for top-level call expressions
	// whose method identifier matches a callback macro.
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "class" {
			nameNode := n.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			className := nameNode.Content(src)
			classID := filePath + "::" + className

			// Actions = instance methods of this class. Build a quick
			// map from method name to node ID so callbacks can be
			// resolved locally; avoids the resolver pass entirely for
			// this synthetic edge.
			methodIDs := make(map[string]string)
			var bodyStatements *sitter.Node
			for i := 0; i < int(n.NamedChildCount()); i++ {
				c := n.NamedChild(i)
				if c != nil && c.Type() == "body_statement" {
					bodyStatements = c
					break
				}
			}
			if bodyStatements == nil {
				return
			}
			// Collect methods first so callback macros can resolve
			// symbol names to concrete IDs.
			for i := 0; i < int(bodyStatements.NamedChildCount()); i++ {
				c := bodyStatements.NamedChild(i)
				if c == nil {
					continue
				}
				if c.Type() == "method" || c.Type() == "singleton_method" {
					nn := c.ChildByFieldName("name")
					if nn == nil {
						continue
					}
					name := nn.Content(src)
					methodIDs[name] = filePath + "::" + className + "." + name
				}
			}
			// First pass: collect every callback method named anywhere
			// in the class's before/after/around macros. These must be
			// excluded from the action set of EVERY macro — otherwise
			// `before_action :a; before_action :b` ends up binding a
			// to guard b and vice versa.
			allCallbacks := make(map[string]struct{})
			for i := 0; i < int(bodyStatements.NamedChildCount()); i++ {
				c := bodyStatements.NamedChild(i)
				if c == nil || c.Type() != "call" {
					continue
				}
				methodNode := c.ChildByFieldName("method")
				if methodNode == nil {
					continue
				}
				if _, ok := railsCallbackMethods[methodNode.Content(src)]; !ok {
					continue
				}
				args := c.ChildByFieldName("arguments")
				if args == nil {
					continue
				}
				for i := 0; i < int(args.NamedChildCount()); i++ {
					arg := args.NamedChild(i)
					if arg != nil && arg.Type() == "simple_symbol" {
						allCallbacks[strings.TrimPrefix(arg.Content(src), ":")] = struct{}{}
					}
				}
			}
			// Second pass: emit edges.
			for i := 0; i < int(bodyStatements.NamedChildCount()); i++ {
				c := bodyStatements.NamedChild(i)
				if c == nil || c.Type() != "call" {
					continue
				}
				methodNode := c.ChildByFieldName("method")
				if methodNode == nil {
					continue
				}
				macro := methodNode.Content(src)
				if _, ok := railsCallbackMethods[macro]; !ok {
					continue
				}
				args := c.ChildByFieldName("arguments")
				if args == nil {
					continue
				}
				emitRailsCallbackEdges(args, src, filePath, int(c.StartPoint().Row)+1, classID, className, methodIDs, allCallbacks, macro, result)
			}
			return
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
}

// emitRailsCallbackEdges pulls symbol args out of a callback macro call,
// applies only:/except: filters against the class's action methods,
// and emits one EdgeCalls per (action, callback) pair. Class-level
// callbacks without only:/except: fan out to every action.
func emitRailsCallbackEdges(args *sitter.Node, src []byte, filePath string, line int, classID, className string, methodIDs map[string]string, allCallbacks map[string]struct{}, macro string, result *parser.ExtractionResult) {
	var callbackSyms []string
	onlyFilter := map[string]struct{}{}
	exceptFilter := map[string]struct{}{}
	hasOnly := false
	hasExcept := false
	for i := 0; i < int(args.NamedChildCount()); i++ {
		arg := args.NamedChild(i)
		if arg == nil {
			continue
		}
		switch arg.Type() {
		case "simple_symbol":
			// `:name` — the most common form.
			sym := strings.TrimPrefix(arg.Content(src), ":")
			callbackSyms = append(callbackSyms, sym)
		case "pair":
			// `only: :show` or `except: [:a, :b]`.
			keyNode := arg.ChildByFieldName("key")
			valNode := arg.ChildByFieldName("value")
			if keyNode == nil || valNode == nil {
				continue
			}
			key := strings.TrimSuffix(strings.TrimPrefix(keyNode.Content(src), ":"), ":")
			target := &onlyFilter
			set := &hasOnly
			switch key {
			case "only":
				// use default onlyFilter
			case "except":
				target = &exceptFilter
				set = &hasExcept
			default:
				continue
			}
			for _, sym := range collectRubySymbols(valNode, src) {
				(*target)[sym] = struct{}{}
			}
			if len(*target) > 0 {
				*set = true
			}
		case "hash":
			// Older Ruby fat-comma syntax (`only => :show`). Rare in
			// modern Rails; skip for simplicity.
		}
	}
	if len(callbackSyms) == 0 {
		return
	}

	// Resolve the actions this macro applies to.
	var applyTo []string
	for name := range methodIDs {
		if hasOnly {
			if _, ok := onlyFilter[name]; !ok {
				continue
			}
		}
		if hasExcept {
			if _, ok := exceptFilter[name]; ok {
				continue
			}
		}
		// Exclude ALL callback methods — a before_action can never
		// guard another before_action's method (Rails fires them all
		// sequentially, each bound to *actions*, not to each other).
		if _, isCallback := allCallbacks[name]; isCallback {
			continue
		}
		applyTo = append(applyTo, name)
	}
	if len(applyTo) == 0 {
		return
	}
	for _, cb := range callbackSyms {
		target := methodIDs[cb]
		if target == "" {
			// Inherited callback (defined on a parent class). Emit
			// an unresolved:: target and let the resolver find it by
			// name — works when the parent is in the same repo.
			target = "unresolved::" + cb
		}
		for _, action := range applyTo {
			actionID := methodIDs[action]
			if actionID == "" {
				continue
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     actionID,
				To:       target,
				Kind:     graph.EdgeCalls,
				FilePath: filePath,
				Line:     line,
				Meta: map[string]any{
					"dispatch_macro": macro,
					"rails_callback": cb,
				},
			})
		}
	}
	_ = classID
	_ = className
}

// collectRubySymbols gathers bare symbol tokens from an expression that
// may be a single symbol (`:foo`) or an array of them (`[:a, :b]`).
func collectRubySymbols(n *sitter.Node, src []byte) []string {
	var out []string
	switch n.Type() {
	case "simple_symbol":
		out = append(out, strings.TrimPrefix(n.Content(src), ":"))
	case "array":
		for i := 0; i < int(n.NamedChildCount()); i++ {
			c := n.NamedChild(i)
			if c != nil && c.Type() == "simple_symbol" {
				out = append(out, strings.TrimPrefix(c.Content(src), ":"))
			}
		}
	}
	return out
}

func isUpperASCII(b byte) bool {
	return b >= 'A' && b <= 'Z'
}

// Ensure RubyExtractor satisfies the Extractor interface at compile time.
var _ parser.Extractor = (*RubyExtractor)(nil)

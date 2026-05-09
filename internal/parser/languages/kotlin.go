package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/kotlin"
)

// qKotlinAll is a single tree-sitter query alternating over every
// pattern the Kotlin extractor needs. One tree walk per file replaces
// the 10 `parser.RunQuery` calls plus the 5 walkNodes traversals the
// previous design made. Capture names are disjoint across patterns so
// the dispatch in Extract can branch on which name is set. Member
// containment for class/object methods, the class-vs-interface-vs-enum
// distinction, and enum entries are all resolved inline by inspecting
// the captured node — the legacy walkNodes pass over class_declaration
// nodes is collapsed into the alternation dispatch.
const qKotlinAll = `
[
  (class_declaration
    (type_identifier) @class.name) @class.def

  (object_declaration
    (type_identifier) @obj.name) @obj.def

  (function_declaration
    (simple_identifier) @func.name) @func.def

  (property_declaration
    (variable_declaration
      (simple_identifier) @prop.name)) @prop.def

  (property_declaration
    (variable_declaration
      (simple_identifier) @tprop.name
      (user_type) @tprop.type)) @tprop.def

  (import_header
    (identifier) @import.path) @import.def

  (call_expression
    (simple_identifier) @call.name) @call.expr

  (call_expression
    (navigation_expression
      (_) @callm.receiver
      (navigation_suffix
        (simple_identifier) @callm.method))) @callm.expr
]
`

// KotlinExtractor extracts Kotlin source files into graph nodes and edges.
type KotlinExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewKotlinExtractor() *KotlinExtractor {
	lang := kotlin.GetLanguage()
	return &KotlinExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qKotlinAll, lang),
	}
}

func (e *KotlinExtractor) Language() string     { return "kotlin" }
func (e *KotlinExtractor) Extensions() []string { return []string{".kt", ".kts"} }

// --- Deferred match buffers ----------------------------------------

type kotlinDeferredCall struct {
	name     string
	receiver string
	line     int
	isMember bool
}

// kotlinDeferredProperty buffers a property_declaration for the
// post-pass type-env build. Mirrors legacy precedence: Tier 0
// (explicit user_type) overwrites; Tier 1 (`val x = Foo()`) only
// fills in keys without an explicit type.
type kotlinDeferredProperty struct {
	name        string
	explicit    string       // normalized type from explicit annotation, "" if none
	defNode     *sitter.Node // property_declaration node, for Tier 1 walk
	atSourceTop bool         // direct child of source_file → emit as top-level var
	line        int
	endLine     int
}

func (e *KotlinExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "kotlin",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	annotationSeen := make(map[string]bool)

	var calls []kotlinDeferredCall
	var props []kotlinDeferredProperty

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["class.def"] != nil:
			e.emitClassOrInterface(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["obj.def"] != nil:
			e.emitObject(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["tprop.def"] != nil:
			// Tier 0 of tenv arrives via this disjoint pattern; we still
			// also see the same property via prop.def below for top-level
			// node emission and Tier 1 fallback.
			name := m.Captures["tprop.name"].Text
			typeName := normalizeKotlinTypeName(m.Captures["tprop.type"].Text)
			if typeName != "" {
				// Stash on the matching prop entry by appending a sentinel;
				// we'll merge in the post-pass. Use a separate slice keyed
				// by name to avoid relying on capture-arrival order.
				props = append(props, kotlinDeferredProperty{
					name:     name,
					explicit: typeName,
				})
			}

		case m.Captures["prop.def"] != nil:
			def := m.Captures["prop.def"]
			top := def.Node != nil && def.Node.Parent() != nil && def.Node.Parent().Type() == "source_file"
			props = append(props, kotlinDeferredProperty{
				name:        m.Captures["prop.name"].Text,
				defNode:     def.Node,
				atSourceTop: top,
				line:        def.StartLine + 1,
				endLine:     def.EndLine + 1,
			})

		case m.Captures["import.def"] != nil:
			e.emitImport(m, filePath, fileID, result)

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			calls = append(calls, kotlinDeferredCall{
				name:     m.Captures["callm.method"].Text,
				receiver: m.Captures["callm.receiver"].Text,
				line:     expr.StartLine + 1,
				isMember: true,
			})

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, kotlinDeferredCall{
				name: m.Captures["call.name"].Text,
				line: expr.StartLine + 1,
			})
		}
	})

	// Build type environment in legacy precedence (Tier 0 overwrites,
	// Tier 1 only fills gaps), and emit top-level property nodes from
	// the same buffer.
	tenv := make(typeEnv)
	for _, p := range props {
		if p.explicit != "" {
			tenv[p.name] = p.explicit
		}
	}
	for _, p := range props {
		if p.explicit != "" {
			continue
		}
		if _, exists := tenv[p.name]; exists {
			continue
		}
		if p.defNode == nil {
			continue
		}
		walkNodes(p.defNode, func(n *sitter.Node) {
			if n.Type() != "call_expression" || n.NamedChildCount() == 0 {
				return
			}
			nameNode := n.NamedChild(0)
			if nameNode == nil || nameNode.Type() != "simple_identifier" {
				return
			}
			funcName := nameNode.Content(src)
			if len(funcName) > 0 && funcName[0] >= 'A' && funcName[0] <= 'Z' {
				tenv[p.name] = funcName
			}
		})
	}
	for _, p := range props {
		if !p.atSourceTop {
			continue
		}
		id := filePath + "::" + p.name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindVariable, Name: p.name,
			FilePath: filePath, StartLine: p.line, EndLine: p.endLine,
			Language: "kotlin",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: p.line,
		})
	}

	// Resolve calls against funcRanges + tenv.
	funcRanges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::*." + c.name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		}
		if c.isMember {
			if recvType, ok := tenv[c.receiver]; ok {
				edge.Meta = map[string]any{"receiver_type": recvType}
			} else if strings.Contains(c.receiver, ".") || strings.Contains(c.receiver, "(") {
				if chainType := resolveChainType(c.receiver, tenv, result); chainType != "" {
					edge.Meta = map[string]any{"receiver_type": chainType}
				}
			}
		}
		result.Edges = append(result.Edges, edge)
	}

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

// emitClassOrInterface inspects the children of a class_declaration to
// classify it as a Kotlin class (KindType), interface (KindInterface),
// or enum (KindType + Meta["kind"]="enum"). For enums it also walks
// the enum_class_body to emit one variable node per enum_entry. This
// replaces the legacy extractClassesAndInterfaces walkNodes pass.
func (e *KotlinExtractor) emitClassOrInterface(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	def := m.Captures["class.def"]
	name := m.Captures["class.name"].Text
	id := filePath + "::" + name
	if seen[id] {
		return
	}

	isInterface := false
	var enumBody *sitter.Node
	if def.Node != nil {
		for i := 0; i < int(def.Node.ChildCount()); i++ {
			child := def.Node.Child(i)
			switch child.Type() {
			case "interface":
				isInterface = true
			case "enum_class_body":
				enumBody = child
			}
		}
	}

	kind := graph.KindType
	meta := map[string]any{"visibility": kotlinVisibility(def.Node, src)}
	switch {
	case isInterface:
		kind = graph.KindInterface
	case enumBody != nil:
		meta["kind"] = "enum"
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}

	seen[id] = true
	startLine := def.StartLine + 1
	endLine := def.EndLine + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: kind, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "kotlin",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	emitKotlinAnnotationEdges(kotlinCollectAnnotations(def.Node, src), id, filePath, result, annotationSeen)
	emitKotlinGenericParamNodes(id, def.Node, src, filePath, startLine, result)

	if enumBody == nil {
		return
	}
	for i := 0; i < int(enumBody.ChildCount()); i++ {
		entry := enumBody.Child(i)
		if entry == nil || entry.Type() != "enum_entry" {
			continue
		}
		var entryName string
		for j := 0; j < int(entry.ChildCount()); j++ {
			ch := entry.Child(j)
			if ch != nil && ch.Type() == "simple_identifier" {
				entryName = ch.Content(src)
				break
			}
		}
		if entryName == "" {
			continue
		}
		entryID := id + "." + entryName
		entryStart := int(entry.StartPoint().Row) + 1
		entryEnd := int(entry.EndPoint().Row) + 1
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: entryID, Kind: graph.KindVariable, Name: entryName,
			FilePath: filePath, StartLine: entryStart, EndLine: entryEnd,
			Language: "kotlin",
			Meta:     map[string]any{"receiver": name, "kind": "enum_entry"},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: entryID, To: id, Kind: graph.EdgeMemberOf,
			FilePath: filePath, Line: entryStart,
		})
	}
}

func (e *KotlinExtractor) emitObject(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["obj.name"].Text
	def := m.Captures["obj.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": kotlinVisibility(def.Node, src)}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "kotlin",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitKotlinAnnotationEdges(kotlinCollectAnnotations(def.Node, src), id, filePath, result, annotationSeen)
}

// emitFunction classifies each function_declaration by its enclosing
// container — a direct child of class_body whose grandparent is a
// class_declaration emits as a class method; class_body of an
// object_declaration emits as an object method; anything else is a
// top-level (free) function. This mirrors the legacy nested
// kotlinQClassMethod / kotlinQObjectMethod pair plus the
// kotlinQFunction fallback's per-line dedupe.
func (e *KotlinExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	startLine1 := def.StartLine + 1
	endLine1 := def.EndLine + 1

	doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar)
	visibility := kotlinVisibility(def.Node, src)

	owner, ownerKind := kotlinDirectMemberOwner(def.Node, src)
	if ownerKind != "" {
		id := filePath + "::" + owner + "." + name
		if seen[id] {
			id = filePath + "::" + owner + "." + name + "_L" + fmt.Sprint(startLine1)
		}
		if seen[id] {
			return
		}
		seen[id] = true
		meta := map[string]any{
			"receiver":   owner,
			"visibility": visibility,
		}
		if doc != "" {
			meta["doc"] = doc
		}
		if rt := extractKotlinReturnType(def.Node, src); rt != "" {
			meta["return_type"] = rt
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: startLine1, EndLine: endLine1,
			Language: "kotlin",
			Meta:     meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
		})
		ownerID := filePath + "::" + owner
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
		})
		emitKotlinAnnotationEdges(kotlinCollectAnnotations(def.Node, src), id, filePath, result, annotationSeen)
		if body := kotlinFunctionBody(def.Node); body != nil {
			emitKotlinAsyncSpawns(id, body, src, filePath, result)
		}
		return
	}

	// Top-level (or nested-in-fn) — emit as KindFunction.
	id := filePath + "::" + name
	if seen[id] {
		id = filePath + "::" + name + "_L" + fmt.Sprint(startLine1)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": visibility}
	if doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: endLine1,
		Language: "kotlin",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	emitKotlinAnnotationEdges(kotlinCollectAnnotations(def.Node, src), id, filePath, result, annotationSeen)
	if body := kotlinFunctionBody(def.Node); body != nil {
		emitKotlinAsyncSpawns(id, body, src, filePath, result)
	}
}

// kotlinCollectAnnotations walks a Kotlin declaration's modifiers
// child for annotation nodes and returns the bare annotation names
// plus their args (typically `(...)` text after the annotation name).
func kotlinCollectAnnotations(decl *sitter.Node, src []byte) []javaAnnotation {
	if decl == nil {
		return nil
	}
	var out []javaAnnotation
	for i := 0; i < int(decl.ChildCount()); i++ {
		c := decl.Child(i)
		if c == nil || c.Type() != "modifiers" {
			continue
		}
		for j := 0; j < int(c.ChildCount()); j++ {
			ann := c.Child(j)
			if ann == nil {
				continue
			}
			// Kotlin grammar exposes annotations as `annotation`
			// children containing a `user_type` (the name) and an
			// optional `value_arguments` (the parens).
			if ann.Type() != "annotation" {
				continue
			}
			var name, args string
			line := int(ann.StartPoint().Row) + 1
			for k := 0; k < int(ann.ChildCount()); k++ {
				inner := ann.Child(k)
				if inner == nil {
					continue
				}
				switch inner.Type() {
				case "user_type", "constructor_invocation":
					if name == "" {
						name = strings.TrimSpace(inner.Content(src))
					}
				case "value_arguments":
					txt := inner.Content(src)
					if len(txt) >= 2 && txt[0] == '(' && txt[len(txt)-1] == ')' {
						txt = txt[1 : len(txt)-1]
					}
					args = txt
				}
			}
			if name == "" {
				continue
			}
			// strip a "()" suffix that the grammar may wrap into the user_type.
			if idx := strings.Index(name, "("); idx >= 0 {
				if args == "" {
					args = strings.TrimSuffix(name[idx+1:], ")")
				}
				name = name[:idx]
			}
			out = append(out, javaAnnotation{name: strings.TrimSpace(name), args: args, line: line})
		}
	}
	return out
}

func emitKotlinAnnotationEdges(anns []javaAnnotation, fromID, filePath string, result *parser.ExtractionResult, seen map[string]bool) {
	for _, a := range anns {
		if a.name == "" {
			continue
		}
		EmitAnnotationEdge(fromID, "kotlin", a.name, a.args, filePath, a.line, result, seen)
	}
}

// kotlinVisibility scans a declaration's modifiers child for a
// visibility modifier. Kotlin's default visibility is "public" when
// none is declared (different from Java's package-private default).
func kotlinVisibility(decl *sitter.Node, src []byte) string {
	if decl == nil {
		return VisibilityPublic
	}
	for i := 0; i < int(decl.ChildCount()); i++ {
		c := decl.Child(i)
		if c == nil || c.Type() != "modifiers" {
			continue
		}
		for j := 0; j < int(c.ChildCount()); j++ {
			tok := c.Child(j)
			if tok == nil {
				continue
			}
			if tok.Type() != "visibility_modifier" {
				continue
			}
			switch strings.TrimSpace(tok.Content(src)) {
			case "public", "open":
				return VisibilityPublic
			case "private":
				return VisibilityPrivate
			case "protected":
				return VisibilityProtected
			case "internal":
				return VisibilityInternal
			}
		}
	}
	return VisibilityPublic
}

func (e *KotlinExtractor) emitImport(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	path := m.Captures["import.path"]
	importPath := strings.ReplaceAll(path.Text, ".", "/")
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + importPath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: path.StartLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

// kotlinDirectMemberOwner returns the (name, container kind) of the
// nearest class_declaration / object_declaration whose body directly
// contains fn. Returns ("", "") for free functions and nested
// functions — preserving the legacy nested-query semantics.
func kotlinDirectMemberOwner(fn *sitter.Node, src []byte) (string, string) {
	if fn == nil {
		return "", ""
	}
	parent := fn.Parent()
	if parent == nil || parent.Type() != "class_body" {
		return "", ""
	}
	grand := parent.Parent()
	if grand == nil {
		return "", ""
	}
	gtype := grand.Type()
	if gtype != "class_declaration" && gtype != "object_declaration" {
		return "", ""
	}
	for i := 0; i < int(grand.ChildCount()); i++ {
		ch := grand.Child(i)
		if ch != nil && ch.Type() == "type_identifier" {
			return ch.Content(src), gtype
		}
	}
	return "", ""
}

// normalizeKotlinTypeName strips generics and nullable markers from a Kotlin type name.
func normalizeKotlinTypeName(t string) string {
	t = strings.TrimSpace(t)
	// Remove nullable suffix.
	t = strings.TrimSuffix(t, "?")
	// Remove generics.
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	// Skip Kotlin primitives.
	switch t {
	case "Int", "Long", "Short", "Byte", "Float", "Double", "Boolean",
		"Char", "String", "Unit", "Any", "Nothing":
		return ""
	}
	if t == "" || (t[0] >= 'a' && t[0] <= 'z') {
		return ""
	}
	return t
}

// extractKotlinReturnType walks a function_declaration node to find the return type annotation.
// Kotlin functions have optional `: ReturnType` after the parameter list.
func extractKotlinReturnType(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	// Look for user_type or nullable_type child after the function_value_parameters.
	pastParams := false
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "function_value_parameters" {
			pastParams = true
			continue
		}
		if pastParams {
			switch child.Type() {
			case "user_type", "nullable_type":
				rawType := string(src[child.StartByte():child.EndByte()])
				if rt := normalizeKotlinTypeName(rawType); rt != "" {
					return rt
				}
			case "function_body":
				// Stop looking once we hit the body.
				return ""
			}
		}
	}
	return ""
}

// walkNodes does a depth-first walk of the tree-sitter node tree.
// Shared with other language extractors via package scope.
func walkNodes(node *sitter.Node, fn func(*sitter.Node)) {
	fn(node)
	for i := 0; i < int(node.ChildCount()); i++ {
		walkNodes(node.Child(i), fn)
	}
}

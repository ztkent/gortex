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

// kotlinLambdaScope records the parameters bound by one lambda literal
// over the line span of its body. A use of such a parameter inside the
// span is locally bound and must not be resolved against the outer type
// environment (which would mis-attribute a `receiver_type` to an
// unrelated outer variable / type of the same name). Implicit `it` is
// always in scope inside a lambda that declares no explicit params.
type kotlinLambdaScope struct {
	params    map[string]bool
	startLine int
	endLine   int
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

	// Companion-object properties (`companion object { const val NAME; val x }`)
	// are statically accessible on the enclosing TYPE (`Foo.NAME`). Emit
	// them as members of that type so the constant / property is
	// discoverable, mirroring the companion-function attribution above.
	for _, p := range props {
		if p.defNode == nil {
			continue
		}
		owner := kotlinResolveMemberOwner(p.defNode, src)
		if !owner.companion || owner.name == "" {
			continue
		}
		emitKotlinCompanionProperty(p, owner, filePath, src, result, seen)
	}

	// Type-name set for static (companion-object) dispatch. A call whose
	// receiver names a class / object — or a named-companion alias
	// (`Type.Companion`) — is a static access on the TYPE; attribute the
	// receiver_type to the type name itself so it resolves to the
	// companion's member (emitted with Meta["receiver"] = <enclosing type>).
	typeNames := kotlinCollectTypeNames(result)

	// Lambda-parameter scopes: receivers that name a lambda param are
	// locally bound and must not pick up an outer receiver_type.
	lambdaScopes := collectKotlinLambdaScopes(root, src)

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
		if c.isMember && !kotlinReceiverIsLambdaParam(lambdaScopes, c.receiver, c.line) {
			switch {
			case tenv[c.receiver] != "":
				edge.Meta = map[string]any{"receiver_type": tenv[c.receiver]}
			case typeNames[c.receiver]:
				// Static dispatch: `Foo.create()` / `Foo.Factory.thing()`.
				edge.Meta = map[string]any{"receiver_type": c.receiver}
			case strings.Contains(c.receiver, ".") || strings.Contains(c.receiver, "("):
				if chainType := resolveChainType(c.receiver, tenv, result); chainType != "" {
					edge.Meta = map[string]any{"receiver_type": chainType}
				}
			}
		}
		result.Edges = append(result.Edges, edge)
	}

	// Expo Modules native DSL (Name/Function/AsyncFunction) → synthetic
	// JS-callable method nodes for the Expo bridge synthesizer.
	emitExpoModuleNodes(src, filePath, "kotlin", fileID, result, seen)

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

	ownerInfo := kotlinResolveMemberOwner(def.Node, src)
	owner, ownerKind := ownerInfo.name, ownerInfo.kind
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
		// Companion-object members dispatch on the TYPE (`Foo.create()`),
		// so the method is attributed to the enclosing class and flagged
		// static. Without this an agent can't see that the companion's
		// `create` is reachable via the class name.
		if ownerInfo.companion {
			meta["static"] = true
			meta["companion"] = true
			if ownerInfo.companionName != "" {
				meta["companion_name"] = ownerInfo.companionName
			}
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

		// Named companion: `companion object Factory { fun thing() }` is
		// callable as both `Foo.thing()` (handled above via the enclosing
		// owner) and `Foo.Factory.thing()`. Emit an alias method whose
		// receiver is `Foo.Factory` so the qualified call resolves too.
		if ownerInfo.companion && ownerInfo.companionName != "" {
			aliasRecv := owner + "." + ownerInfo.companionName
			aliasID := filePath + "::" + aliasRecv + "." + name
			if !seen[aliasID] {
				seen[aliasID] = true
				aliasMeta := map[string]any{
					"receiver":        aliasRecv,
					"visibility":      visibility,
					"static":          true,
					"companion":       true,
					"companion_name":  ownerInfo.companionName,
					"companion_alias": true,
				}
				if doc != "" {
					aliasMeta["doc"] = doc
				}
				if rt := extractKotlinReturnType(def.Node, src); rt != "" {
					aliasMeta["return_type"] = rt
				}
				result.Nodes = append(result.Nodes, &graph.Node{
					ID: aliasID, Kind: graph.KindMethod, Name: name,
					FilePath: filePath, StartLine: startLine1, EndLine: endLine1,
					Language: "kotlin",
					Meta:     aliasMeta,
				})
				result.Edges = append(result.Edges, &graph.Edge{
					From: aliasID, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
				})
			}
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

// kotlinMemberOwner describes the container a declaration belongs to.
// For a plain class/object method, name is the type name and
// companionName is empty. For a companion-object member, name is the
// ENCLOSING class (so `Foo.create()` resolves to the companion's
// create) and companionName carries the companion's declared name —
// empty for an anonymous `companion object`, the identifier for a named
// one like `companion object Factory`.
type kotlinMemberOwner struct {
	name          string // enclosing type/object name
	kind          string // "class_declaration" | "object_declaration"
	companion     bool   // member lives in a companion object
	companionName string // declared companion name, "" if anonymous
}

// kotlinResolveMemberOwner walks up from a declaration to find its
// container. It transparently sees through a companion_object wrapper:
// `class Foo { companion object Factory { fun create() {} } }` resolves
// the owner of create to Foo while recording companion=true and
// companionName="Factory". This is what makes `Foo.create()` (static
// dispatch on the type) discoverable.
func kotlinResolveMemberOwner(fn *sitter.Node, src []byte) kotlinMemberOwner {
	if fn == nil {
		return kotlinMemberOwner{}
	}
	parent := fn.Parent()
	if parent == nil || parent.Type() != "class_body" {
		return kotlinMemberOwner{}
	}
	grand := parent.Parent()
	if grand == nil {
		return kotlinMemberOwner{}
	}

	// Companion-object member: `class Foo { companion object [Name] { ... } }`.
	// The grandparent is the companion_object; its enclosing class is two
	// more levels up (class_body → class_declaration / object_declaration).
	if grand.Type() == "companion_object" {
		companionName := kotlinTypeIdentifierChild(grand, src)
		outerBody := grand.Parent()
		if outerBody == nil || outerBody.Type() != "class_body" {
			return kotlinMemberOwner{}
		}
		outer := outerBody.Parent()
		if outer == nil {
			return kotlinMemberOwner{}
		}
		otype := outer.Type()
		if otype != "class_declaration" && otype != "object_declaration" {
			return kotlinMemberOwner{}
		}
		name := kotlinTypeIdentifierChild(outer, src)
		if name == "" {
			return kotlinMemberOwner{}
		}
		return kotlinMemberOwner{
			name:          name,
			kind:          otype,
			companion:     true,
			companionName: companionName,
		}
	}

	gtype := grand.Type()
	if gtype != "class_declaration" && gtype != "object_declaration" {
		return kotlinMemberOwner{}
	}
	name := kotlinTypeIdentifierChild(grand, src)
	if name == "" {
		return kotlinMemberOwner{}
	}
	return kotlinMemberOwner{name: name, kind: gtype}
}

// kotlinTypeIdentifierChild returns the first type_identifier child of
// node (the declared name of a class/object/companion), or "" when the
// node is anonymous.
func kotlinTypeIdentifierChild(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	for i := 0; i < int(node.ChildCount()); i++ {
		ch := node.Child(i)
		if ch != nil && ch.Type() == "type_identifier" {
			return ch.Content(src)
		}
	}
	return ""
}

// kotlinCollectTypeNames returns the set of receiver strings that name a
// type for the purpose of static (companion-object) dispatch: every
// class / object / interface name, plus the `Type.Companion` aliases
// emitted for named companion objects (so `Bar.Factory.thing()` lands on
// the alias method's receiver). The companion alias receivers are read
// back from the alias method nodes' Meta["receiver"].
func kotlinCollectTypeNames(result *parser.ExtractionResult) map[string]bool {
	names := map[string]bool{}
	for _, n := range result.Nodes {
		switch n.Kind {
		case graph.KindType, graph.KindInterface:
			if n.Name != "" {
				names[n.Name] = true
			}
		case graph.KindMethod:
			if n.Meta == nil {
				continue
			}
			if alias, _ := n.Meta["companion_alias"].(bool); !alias {
				continue
			}
			if recv, _ := n.Meta["receiver"].(string); recv != "" {
				names[recv] = true
			}
		}
	}
	return names
}

// emitKotlinCompanionProperty emits one member node for a property
// declared inside a companion object, attributed to the enclosing type
// so `Foo.NAME` is discoverable. A `const`-modified property is a
// KindConstant; a plain `val`/`var` is a KindField. Both carry
// Meta["receiver"] = <enclosing type> and Meta["static"] = true. For a
// named companion an alias member (`Type.Companion.NAME`) is added too.
func emitKotlinCompanionProperty(p kotlinDeferredProperty, owner kotlinMemberOwner, filePath string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	kind := graph.KindField
	if kotlinPropertyIsConst(p.defNode, src) {
		kind = graph.KindConstant
	}
	emit := func(recv string) {
		id := filePath + "::" + recv + "." + p.name
		if seen[id] {
			return
		}
		seen[id] = true
		meta := map[string]any{
			"receiver":  recv,
			"static":    true,
			"companion": true,
		}
		if owner.companionName != "" {
			meta["companion_name"] = owner.companionName
		}
		ownerID := filePath + "::" + owner.name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: kind, Name: p.name,
			FilePath: filePath, StartLine: p.line, EndLine: p.endLine,
			Language: "kotlin",
			Meta:     meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: p.line,
		})
	}
	emit(owner.name)
	if owner.companionName != "" {
		emit(owner.name + "." + owner.companionName)
	}
}

// kotlinPropertyIsConst reports whether a property_declaration carries a
// `const` modifier.
func kotlinPropertyIsConst(decl *sitter.Node, src []byte) bool {
	if decl == nil {
		return false
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
			if strings.TrimSpace(tok.Content(src)) == "const" {
				return true
			}
		}
	}
	return false
}

// collectKotlinLambdaScopes walks the tree for lambda_literal nodes and
// records, for each, the parameter names it binds plus the line span of
// its body. Explicit params come from the `lambda_parameters` child
// (`{ a, b -> ... }`); a lambda with no declared params implicitly binds
// `it` (`list.forEach { println(it) }`). These scopes let the call
// resolver recognise that a member call whose receiver is a lambda
// parameter is locally bound, so it is not mis-resolved against the
// outer type environment.
func collectKotlinLambdaScopes(root *sitter.Node, src []byte) []kotlinLambdaScope {
	var scopes []kotlinLambdaScope
	walkNodes(root, func(n *sitter.Node) {
		if n == nil || n.Type() != "lambda_literal" {
			return
		}
		params := map[string]bool{}
		hasExplicit := false
		for i := 0; i < int(n.ChildCount()); i++ {
			ch := n.Child(i)
			if ch == nil || ch.Type() != "lambda_parameters" {
				continue
			}
			hasExplicit = true
			// lambda_parameters → variable_declaration → simple_identifier,
			// possibly several (destructuring / multi-param).
			walkNodes(ch, func(p *sitter.Node) {
				if p != nil && p.Type() == "simple_identifier" {
					params[p.Content(src)] = true
				}
			})
		}
		if !hasExplicit {
			// No declared params: the implicit single parameter `it`.
			params["it"] = true
		}
		if len(params) == 0 {
			return
		}
		scopes = append(scopes, kotlinLambdaScope{
			params:    params,
			startLine: int(n.StartPoint().Row) + 1,
			endLine:   int(n.EndPoint().Row) + 1,
		})
	})
	return scopes
}

// kotlinReceiverIsLambdaParam reports whether the head identifier of a
// receiver expression names a lambda parameter that is in scope at the
// call's line. The head is the first dotted segment, so both `it` and
// `item` (in `item.x`) are caught.
func kotlinReceiverIsLambdaParam(scopes []kotlinLambdaScope, receiver string, line int) bool {
	head := receiver
	if idx := strings.IndexByte(head, '.'); idx >= 0 {
		head = head[:idx]
	}
	if idx := strings.IndexByte(head, '('); idx >= 0 {
		head = head[:idx]
	}
	head = strings.TrimSpace(head)
	if head == "" {
		return false
	}
	for _, s := range scopes {
		if line < s.startLine || line > s.endLine {
			continue
		}
		if s.params[head] {
			return true
		}
	}
	return false
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

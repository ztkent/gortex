package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Makefile is target-and-recipe structured: a target line `NAME:` is
// followed by tab-indented recipe lines. We model targets and
// `define NAME ... endef` blocks as function nodes, variable
// assignments as variables, and `include` / `-include` / `sinclude`
// directives as imports.
var (
	makeTargetRe  = regexp.MustCompile(`(?m)^([A-Za-z_][\w./-]*(?:\.[A-Za-z_][\w./-]*)?)\s*:(?:[^=]|$)`)
	makeDefineRe  = regexp.MustCompile(`(?m)^define\s+([A-Za-z_]\w*)`)
	makeIncludeRe = regexp.MustCompile(`(?m)^(?:-include|sinclude|include)\s+(.+)$`)
	makeVarRe     = regexp.MustCompile(`(?m)^([A-Za-z_][\w]*)\s*(?::=|\?=|\+=|=)\s*`)
)

// MakefileExtractor extracts Makefile source using regex.
type MakefileExtractor struct{}

func NewMakefileExtractor() *MakefileExtractor { return &MakefileExtractor{} }

func (e *MakefileExtractor) Language() string { return "makefile" }
func (e *MakefileExtractor) Extensions() []string {
	return []string{".mk", ".make", "Makefile", "GNUmakefile", "makefile"}
}

func (e *MakefileExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	lines := strings.Split(string(src), "\n")
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: len(lines),
		Language: "makefile",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	add := func(name string, kind graph.NodeKind, start, end int) {
		if name == "" {
			return
		}
		id := filePath + "::" + name
		if seen[id] {
			return
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: kind, Name: name,
			FilePath: filePath, StartLine: start, EndLine: end,
			Language: "makefile",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: start,
		})
	}

	// Collect target lines to compute end = line before next top-level
	// definition (targets have no explicit terminator; indented tab
	// lines are the recipe).
	var tops []makeTopHit
	for _, m := range makeTargetRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		if isMakeDirective(name) {
			continue
		}
		line := lineAt(src, m[0])
		tops = append(tops, makeTopHit{name: name, line: line, kind: graph.KindFunction})
	}
	for _, m := range makeVarRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		if isMakeDirective(name) {
			continue
		}
		line := lineAt(src, m[0])
		tops = append(tops, makeTopHit{name: name, line: line, kind: graph.KindVariable})
	}
	// Sort-by-line so end-of-range computation is monotonic.
	for i := 0; i < len(tops); i++ {
		for j := i + 1; j < len(tops); j++ {
			if tops[j].line < tops[i].line {
				tops[i], tops[j] = tops[j], tops[i]
			}
		}
	}
	for i, t := range tops {
		endLine := len(lines)
		if i+1 < len(tops) {
			endLine = tops[i+1].line - 1
			if endLine < t.line {
				endLine = t.line
			}
		}
		add(t.name, t.kind, t.line, endLine)
	}

	for _, m := range makeDefineRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		add(name, graph.KindFunction, line, findKeywordBlockEnd(lines, line, "endef"))
	}

	for _, m := range makeIncludeRe.FindAllSubmatchIndex(src, -1) {
		arg := strings.TrimSpace(string(src[m[2]:m[3]]))
		line := lineAt(src, m[0])
		// `include a.mk b.mk` may list several files.
		for _, f := range strings.Fields(arg) {
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: "unresolved::import::" + f,
				Kind: graph.EdgeImports, FilePath: filePath, Line: line,
			})
		}
	}

	emitMakeRecipeCallEdges(filePath, lines, tops, result)

	return result, nil
}

// emitMakeRecipeCallEdges walks each target's recipe lines and emits
// EdgeCalls when a recipe invokes another target in the same file.
// Recognised forms:
//
//   $(MAKE) <target>   →  emit edge to <target>
//   ${MAKE} <target>   →  same
//   make <target>      →  same
//   <bare-cmd>         →  emit edge to <bare-cmd> when it matches
//                          another target name
//
// Recipe-line modifiers (`@`, `-`, `+`) are stripped. Subshell
// interpolations like `$(shell …)` are skipped — they're command
// substitutions, not direct call edges. External commands (`grep`,
// `git`, etc.) produce no edge unless they happen to match a target
// name; that's a small false-positive risk we accept for the much
// larger build-chain coverage win.
// makeTopHit is the package-scope form of the local topHit struct,
// shared between Extract and emitMakeRecipeCallEdges so the call-edge
// pass can reuse the same target/variable inventory.
type makeTopHit struct {
	name string
	line int
	kind graph.NodeKind
}

func emitMakeRecipeCallEdges(filePath string, lines []string, tops []makeTopHit, result *parser.ExtractionResult) {
	// Build target-name lookup so call resolution is O(1).
	targets := map[string]int{} // name → start line
	for _, t := range tops {
		if t.kind == graph.KindFunction {
			targets[t.name] = t.line
		}
	}
	if len(targets) == 0 {
		return
	}
	// Cache of recipe lines per target.
	for i, t := range tops {
		if t.kind != graph.KindFunction {
			continue
		}
		end := len(lines)
		if i+1 < len(tops) {
			end = tops[i+1].line - 1
			if end < t.line {
				end = t.line
			}
		}
		fromID := filePath + "::" + t.name
		seenCallEdges := map[string]bool{}
		// Recipe lines are indented (tab or space) — not the target
		// header line itself.
		for ln := t.line + 1; ln <= end && ln <= len(lines); ln++ {
			if ln-1 < 0 || ln-1 >= len(lines) {
				continue
			}
			raw := lines[ln-1]
			if !startsWithIndent(raw) {
				continue
			}
			cmd := stripRecipePrefix(raw)
			callee := makeRecipeCallTarget(cmd, targets)
			if callee == "" || callee == t.name {
				continue
			}
			key := callee + "@" + lineToKey(ln)
			if seenCallEdges[key] {
				continue
			}
			seenCallEdges[key] = true
			result.Edges = append(result.Edges, &graph.Edge{
				From:     fromID,
				To:       filePath + "::" + callee,
				Kind:     graph.EdgeCalls,
				FilePath: filePath,
				Line:     ln,
				Origin:   graph.OriginASTInferred,
			})
		}
	}
}

// startsWithIndent reports whether a recipe line begins with a tab or
// space indent. Plain Makefiles use tabs; some projects use leading
// spaces with `.RECIPEPREFIX`. Either is acceptable.
func startsWithIndent(s string) bool {
	if len(s) == 0 {
		return false
	}
	return s[0] == '\t' || s[0] == ' '
}

// stripRecipePrefix removes leading indent and the optional recipe
// modifiers (`@` silent, `-` keep-going, `+` always-execute).
func stripRecipePrefix(s string) string {
	s = strings.TrimLeft(s, " \t")
	for {
		if s == "" {
			return s
		}
		switch s[0] {
		case '@', '-', '+':
			s = s[1:]
		default:
			return s
		}
	}
}

// makeRecipeCallTarget returns the same-file target name a recipe
// command invokes, or "" when the command isn't a target call.
func makeRecipeCallTarget(cmd string, targets map[string]int) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	// `$(MAKE) <target>` / `${MAKE} <target>` / `make <target>`.
	for _, prefix := range []string{"$(MAKE)", "${MAKE}", "make"} {
		if strings.HasPrefix(cmd, prefix) {
			rest := strings.TrimSpace(cmd[len(prefix):])
			// First whitespace-delimited token after the prefix.
			tok := firstWord(rest)
			if tok != "" {
				if _, ok := targets[tok]; ok {
					return tok
				}
			}
			return ""
		}
	}
	// Bare command — emit edge only when it matches an existing
	// target. This avoids `grep`, `git`, etc. producing unresolved
	// edges. Strip any leading `$(...)` interpolation.
	if strings.HasPrefix(cmd, "$(") || strings.HasPrefix(cmd, "${") {
		return ""
	}
	tok := firstWord(cmd)
	if tok == "" {
		return ""
	}
	if _, ok := targets[tok]; ok {
		return tok
	}
	return ""
}

func firstWord(s string) string {
	for i, r := range s {
		if r == ' ' || r == '\t' {
			return s[:i]
		}
	}
	return s
}

func lineToKey(ln int) string {
	return strings.Repeat(" ", ln%8) + "."
}

// isMakeDirective filters out reserved-word collisions that the
// variable regex would otherwise catch.
func isMakeDirective(s string) bool {
	switch s {
	case "ifeq", "ifneq", "ifdef", "ifndef", "else", "endif",
		"define", "endef", "include", "sinclude", "export",
		"unexport", "override", "vpath", "VPATH":
		return true
	}
	return false
}

var _ parser.Extractor = (*MakefileExtractor)(nil)

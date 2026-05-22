package mcp

import (
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"
)

// aliasCanonicals maps a hallucinated / mistyped MCP parameter name to the
// canonical Gortex parameter name(s) an agent most likely intended. A
// rewrite only fires when exactly one candidate is a real, not-yet-set
// parameter of the tool being called (see resolveParamAlias), so a
// multi-target entry resolves itself against the tool's actual schema.
var aliasCanonicals = map[string][]string{
	// identifier
	"symbol":    {"id"},
	"symbol_id": {"id"},
	"symbolid":  {"id"},
	"node":      {"id"},
	"node_id":   {"id"},
	"target":    {"id"},
	// file path
	"file":      {"path"},
	"filepath":  {"path"},
	"file_path": {"path"},
	"filename":  {"path"},
	// query / search text
	"q":       {"query"},
	"search":  {"query"},
	"text":    {"query"},
	"term":    {"query"},
	"keyword": {"query"},
	// edit payloads
	"new_body":    {"new_string", "new_source", "content"},
	"new_content": {"new_string", "new_source", "content"},
	"body":        {"content", "new_string", "new_source"},
	"old_body":    {"old_string", "old_source"},
	"replacement": {"new_string", "new_source"},
	// task / instruction
	"prompt":      {"task"},
	"goal":        {"task"},
	"instruction": {"task"},
}

// paramRewrite records one applied alias rewrite, for debug logging.
type paramRewrite struct{ from, to string }

// reconcileArgKeys rewrites, in place, argument keys that are not real
// parameters of the tool to their canonical names. A key is rewritten
// only when it confidently resolves to exactly one real, not-yet-supplied
// parameter. Returns the rewrites applied.
func reconcileArgKeys(args map[string]any, real map[string]bool) []paramRewrite {
	if len(args) == 0 || len(real) == 0 {
		return nil
	}
	var unknown []string
	for k := range args {
		if !real[k] {
			unknown = append(unknown, k)
		}
	}
	var rewrites []paramRewrite
	for _, k := range unknown {
		target := resolveParamAlias(k, real, args)
		if target == "" {
			continue
		}
		args[target] = args[k]
		delete(args, k)
		rewrites = append(rewrites, paramRewrite{from: k, to: target})
	}
	return rewrites
}

// resolveParamAlias returns the canonical parameter name key was most
// likely meant to be, or "" when there is no confident single match. A
// candidate qualifies only if it is a real parameter of the tool and is
// not already present in args (so an explicit value is never displaced).
func resolveParamAlias(key string, real map[string]bool, args map[string]any) string {
	keyLower := strings.ToLower(strings.TrimSpace(key))
	candidates := map[string]struct{}{}
	for _, c := range aliasCanonicals[keyLower] {
		candidates[c] = struct{}{}
	}
	// Edit-distance typo match against the tool's real parameters.
	for r := range real {
		if len(r) < 4 {
			continue // too short to typo-match safely
		}
		if d := levenshtein(keyLower, strings.ToLower(r)); d > 0 && d <= 2 {
			candidates[r] = struct{}{}
		}
	}
	var viable []string
	for c := range candidates {
		if !real[c] {
			continue
		}
		if _, present := args[c]; present {
			continue
		}
		viable = append(viable, c)
	}
	if len(viable) == 1 {
		return viable[0]
	}
	return ""
}

// levenshtein computes the edit distance between two short strings.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// toolParamNames returns the set of real parameter names declared by the
// named tool's input schema, or nil when the tool or its schema is unknown.
func (s *Server) toolParamNames(toolName string) map[string]bool {
	if s == nil || s.mcpServer == nil {
		return nil
	}
	st := s.mcpServer.GetTool(toolName)
	if st == nil {
		return nil
	}
	props := st.Tool.InputSchema.Properties
	if len(props) == 0 {
		return nil
	}
	out := make(map[string]bool, len(props))
	for k := range props {
		out[k] = true
	}
	return out
}

// reconcileToolParams tolerates hallucinated / mistyped parameter names on
// an incoming tool call: any argument key that is not a real parameter of
// the tool but confidently resolves to one is rewritten in place before
// the handler reads arguments. A no-op when every key is already valid.
func (s *Server) reconcileToolParams(req *mcp.CallToolRequest) {
	if req == nil {
		return
	}
	args, ok := req.Params.Arguments.(map[string]any)
	if !ok || len(args) == 0 {
		return
	}
	real := s.toolParamNames(req.Params.Name)
	if len(real) == 0 {
		return
	}
	for _, rw := range reconcileArgKeys(args, real) {
		if s.logger != nil {
			s.logger.Debug("tool_param_alias: accepted aliased parameter",
				zap.String("tool", req.Params.Name),
				zap.String("from", rw.from),
				zap.String("to", rw.to))
		}
	}
}

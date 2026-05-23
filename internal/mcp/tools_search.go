package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// registerToolsSearch installs the well-known tools_search discovery
// tool that lets clients fetch deferred tool schemas.
//
// Eagerly registered in NewServer so the initial tools/list payload
// always carries it. The tool's description doubles as in-band
// documentation: a client that knows nothing else about Gortex can
// read the description, learn the discovery syntax, and pull the rest
// of the surface on demand.
func (s *Server) registerToolsSearch() {
	if s == nil || s.lazy == nil || !s.lazy.Enabled() {
		// When lazy registration is opted out, we never publish
		// tools_search — every tool is already in tools/list, the
		// discovery tool would just be noise.
		return
	}

	desc := strings.Join([]string{
		"Fetches full schema definitions for deferred tools so they can be called.",
		"",
		"To keep cold-session context lean, the initial tools/list omits low-traffic tool schemas. ",
		"This tool returns matching schemas on demand and promotes them into the live tool set, ",
		"so subsequent tools/call requests dispatch through the normal path.",
		"",
		"Query forms:",
		`  - ""                         — browse mode, returns the names of all currently-deferred tools`,
		`  - "select:foo,bar,baz"       — fetch these exact tools by name`,
		`  - "+slack send"              — require "slack" in the tool name, rank by remaining terms`,
		`  - "memories invariants"      — keyword search across name+description, ranked, capped at max_results`,
		"",
		"Result: each matched tool's full JSON schema, wrapped in <functions>{...}</function> blocks. ",
		"Wrapped tools are auto-promoted into tools/list; the server fires notifications/tools/list_changed ",
		"for any client that subscribes.",
	}, "\n")

	tool := mcp.NewTool(LazyToolsSearchName,
		mcp.WithDescription(desc),
		mcp.WithString("query", mcp.Description(
			"Query string (see description for forms). Empty / omitted lists deferred tool names without schemas (browse mode).",
		)),
		mcp.WithNumber("max_results", mcp.Description(
			"Maximum number of tool schemas to return (default 10, capped at the deferred catalog size). Ignored for select: queries.",
		)),
		mcp.WithBoolean("promote", mcp.Description(
			"When true (default), matched tools are also promoted into tools/list so they can be called directly afterwards. Set to false to return schemas without promotion — useful for read-only inspection.",
		)),
	)

	s.mcpServer.AddTool(tool, s.wrapToolHandler(s.handleToolsSearch))
}

// toolsSearchPayload is the structured form of the discovery tool's
// response. Returned both inline (in the text content) and as the
// canonical body so well-typed clients can parse it directly.
type toolsSearchPayload struct {
	Query        string             `json:"query"`
	Deferred     int                `json:"deferred_remaining"`
	OmittedCount int                `json:"omitted_count,omitempty"`
	Promoted     []string           `json:"promoted,omitempty"`
	BrowseNames  []string           `json:"browse_names,omitempty"`
	Tools        []toolsSearchEntry `json:"tools,omitempty"`
}

type toolsSearchEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

func (s *Server) handleToolsSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s == nil || s.lazy == nil {
		return mcp.NewToolResultError("tools_search: lazy registry not initialised"), nil
	}
	query := strings.TrimSpace(req.GetString("query", ""))
	max := req.GetInt("max_results", 10)
	promote := req.GetBool("promote", true)

	// Browse mode: return the names of all deferred tools so the
	// agent learns the catalog without paying schema bytes upfront.
	if query == "" {
		names := s.lazy.DeferredNames()
		payload := toolsSearchPayload{
			Query:       "",
			Deferred:    len(names),
			BrowseNames: names,
		}
		return renderToolsSearchResult(payload)
	}

	hits, total := s.lazy.QueryWithTotal(query, max)
	entries := make([]toolsSearchEntry, 0, len(hits))
	names := make([]string, 0, len(hits))
	for _, dt := range hits {
		schema, err := json.Marshal(dt.tool.InputSchema)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("tools_search: marshal schema for %q: %v", dt.tool.Name, err)), nil
		}
		entries = append(entries, toolsSearchEntry{
			Name:        dt.tool.Name,
			Description: dt.tool.Description,
			InputSchema: schema,
		})
		names = append(names, dt.tool.Name)
	}

	// Hold the response under the harness's ~50KB wire-size threshold:
	// a select:<many-tools> query asking for 15+ tool schemas easily
	// pushes past 40KB raw, which the stdio bridge then re-escapes and
	// spills to disk. Cap by accumulated byte count so the response is
	// always inline. Dropped entries are reported via OmittedCount;
	// promotion only fires for entries that actually shipped.
	bytesOmitted := 0
	entries, bytesOmitted = trimToolsSearchEntries(entries, defaultMaxBytes)
	if bytesOmitted > 0 {
		names = names[:len(entries)]
	}

	var promotedNames []string
	if promote && len(names) > 0 {
		promotedNames = s.lazy.Promote(names...)
	}

	payload := toolsSearchPayload{
		Query:    query,
		Deferred: s.lazy.CountDeferred(),
		Promoted: promotedNames,
		Tools:    entries,
	}
	// total counts matches before the max cap; a positive remainder
	// (either from max_results or the byte budget) means the agent
	// should narrow the query or use select:<name>.
	if omitted := total - len(entries); omitted > 0 {
		payload.OmittedCount = omitted
	}
	return renderToolsSearchResult(payload)
}

// trimToolsSearchEntries shrinks the entry list so the rendered
// response stays under the transport-safe budget. Estimates per-entry
// bytes from name + description + schema length plus a small
// per-entry overhead for JSON framing. Returns the kept prefix and
// the count of entries dropped from the tail.
func trimToolsSearchEntries(entries []toolsSearchEntry, budget int) ([]toolsSearchEntry, int) {
	if budget <= 0 || len(entries) == 0 {
		return entries, 0
	}
	// Per-entry JSON framing overhead: {"name":"","description":"","input_schema":...}
	// is ~50 bytes of wrappers, key names, and quote/colon punctuation.
	const perEntryOverhead = 60
	bytesUsed := 0
	for i, e := range entries {
		entryBytes := len(e.Name) + len(e.Description) + len(e.InputSchema) + perEntryOverhead
		// Keep at least one entry even if a single schema is enormous —
		// truncating to zero would hide which tool overflowed.
		if i > 0 && bytesUsed+entryBytes > budget {
			return entries[:i], len(entries) - i
		}
		bytesUsed += entryBytes
	}
	return entries, 0
}

// renderToolsSearchResult returns the discovery tool result with both
// the human-readable <functions>{...}</function> block (so an agent
// can parse straight from the text content) and the structured JSON
// payload.
func renderToolsSearchResult(payload toolsSearchPayload) (*mcp.CallToolResult, error) {
	var b strings.Builder
	if len(payload.Tools) > 0 {
		b.WriteString("<functions>\n")
		for _, t := range payload.Tools {
			schema := t.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{}`)
			}
			entry := struct {
				Description string          `json:"description"`
				Name        string          `json:"name"`
				Parameters  json.RawMessage `json:"parameters"`
			}{
				Description: t.Description,
				Name:        t.Name,
				Parameters:  schema,
			}
			line, err := json.Marshal(entry)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("tools_search: marshal entry for %q: %v", t.Name, err)), nil
			}
			b.WriteString("<function>")
			b.Write(line)
			b.WriteString("</function>\n")
		}
		b.WriteString("</functions>\n")
		if payload.OmittedCount > 0 {
			fmt.Fprintf(&b, "\nnote: %d more tool(s) matched but were not returned. Raise max_results, or — if you know the exact tool name — fetch it directly with query \"select:<exact-name>\".\n", payload.OmittedCount)
		}
	} else if len(payload.BrowseNames) > 0 {
		fmt.Fprintf(&b, "%d deferred tool(s) — call tools_search with a query to fetch schemas.\n\n", payload.Deferred)
		for _, n := range payload.BrowseNames {
			b.WriteString("- ")
			b.WriteString(n)
			b.WriteString("\n")
		}
	} else {
		fmt.Fprintf(&b, "tools_search: no deferred tools match %q (deferred_remaining=%d).\nIf you know the exact tool name, retry with query \"select:<exact-name>\".\n", payload.Query, payload.Deferred)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("tools_search: marshal payload: %v", err)), nil
	}
	// Emit text + structured content together. The text carries the
	// <functions> block for prompt-side consumption; the structured
	// body is the canonical machine-readable form.
	res := mcp.NewToolResultText(b.String())
	res.StructuredContent = json.RawMessage(body)
	return res, nil
}

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) registerResources() {
	// Session state: survives context compaction.
	s.mcpServer.AddResource(
		mcp.NewResource(
			"gortex://session",
			"Session State",
			mcp.WithResourceDescription("Recently viewed symbols, modified files, and search queries. Read after context compaction to restore working memory without re-calling tools."),
			mcp.WithMIMEType("text/plain"),
		),
		s.handleResourceSession,
	)

	// Static resource: graph stats (session start orientation). Same
	// payload as the `graph_stats` tool — kept as a tool too for
	// back-compat with clients that don't speak resources.
	s.mcpServer.AddResource(
		mcp.NewResource(
			"gortex://stats",
			"Graph Statistics",
			mcp.WithResourceDescription("Node/edge counts by kind and language, plus per-repo / token-savings / semantic-provider rollups. Read at session start to orient in the codebase. Updates push as `notifications/resources/updated` after each graph re-warm."),
			mcp.WithMIMEType("application/json"),
		),
		s.handleResourceStats,
	)

	// Static resource: graph schema reference.
	s.mcpServer.AddResource(
		mcp.NewResource(
			"gortex://schema",
			"Graph Schema",
			mcp.WithResourceDescription("Node kinds, edge kinds, and their relationships. Reference for understanding graph query results."),
			mcp.WithMIMEType("text/plain"),
		),
		s.handleResourceSchema,
	)

	// Bootstrap-state resources: read-only, no args, every session
	// hits these at startup. Same payloads as the corresponding
	// tools; tools stay registered for back-compat.
	s.registerBootstrapResources()

	// Analyzer-backed rollup resources: long-form summaries whose
	// only "argument" is the current state of the indexed code.
	s.registerAnalyzerResources()

	// Template resources: communities and processes (dynamic, parameterized).
	s.mcpServer.AddResourceTemplate(
		mcp.NewResourceTemplate(
			"gortex://communities",
			"Communities",
			mcp.WithTemplateDescription("Functional clusters discovered by community detection with cohesion scores."),
			mcp.WithTemplateMIMEType("application/json"),
		),
		s.handleResourceCommunities,
	)

	s.mcpServer.AddResourceTemplate(
		mcp.NewResourceTemplate(
			"gortex://community/{id}",
			"Community Detail",
			mcp.WithTemplateDescription("Members, files, and cohesion score for a specific community."),
			mcp.WithTemplateMIMEType("application/json"),
		),
		s.handleResourceCommunity,
	)

	s.mcpServer.AddResourceTemplate(
		mcp.NewResourceTemplate(
			"gortex://processes",
			"Processes",
			mcp.WithTemplateDescription("Discovered execution flows — call chains starting from entry points."),
			mcp.WithTemplateMIMEType("application/json"),
		),
		s.handleResourceProcesses,
	)

	s.mcpServer.AddResourceTemplate(
		mcp.NewResourceTemplate(
			"gortex://process/{id}",
			"Process Detail",
			mcp.WithTemplateDescription("Step-by-step call chain for a specific execution flow."),
			mcp.WithTemplateMIMEType("application/json"),
		),
		s.handleResourceProcess,
	)
}

func (s *Server) handleResourceStats(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	return jsonResource(req.Params.URI, s.buildGraphStatsPayload(ctx))
}

// jsonResource marshals payload as JSON and wraps it in the single-entry
// ResourceContents slice every read-resource handler returns.
func jsonResource(uri string, payload any) ([]mcp.ResourceContents, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (s *Server) handleResourceSession(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	snap := s.sessionFor(ctx).snapshot()

	var b strings.Builder
	b.WriteString("# Gortex Session State\n\n")

	if files, ok := snap["modified_files"].([]string); ok && len(files) > 0 {
		b.WriteString("## Modified Files\n")
		for _, f := range files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n")
	}

	if symbols, ok := snap["viewed_symbols"].([]string); ok && len(symbols) > 0 {
		b.WriteString("## Recently Viewed Symbols\n")
		for _, s := range symbols {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		b.WriteString("\n")
	}

	if files, ok := snap["viewed_files"].([]string); ok && len(files) > 0 {
		b.WriteString("## Recently Viewed Files\n")
		for _, f := range files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n")
	}

	if queries, ok := snap["recent_searches"].([]string); ok && len(queries) > 0 {
		b.WriteString("## Recent Searches\n")
		for _, q := range queries {
			fmt.Fprintf(&b, "- \"%s\"\n", q)
		}
		b.WriteString("\n")
	}

	if b.Len() <= len("# Gortex Session State\n\n") {
		b.WriteString("No activity recorded yet.\n")
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "text/plain",
			Text:     b.String(),
		},
	}, nil
}

func (s *Server) handleResourceSchema(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	schema := `# Gortex Graph Schema

## Node Kinds
- file      — source file
- function  — top-level function or free function
- method    — method belonging to a type (has EdgeMemberOf)
- type      — struct, class, enum, module, table
- interface — interface, trait, protocol, service
- variable  — variable, constant, field, property
- import    — resolved or unresolved import target
- package   — package, namespace, module
- doc       — a heading-delimited Markdown prose section; Name is the
              breadcrumb heading path, Meta["section_text"] holds the
              section body. Searchable via search_symbols corpus:docs.

## Edge Kinds
- calls        — function/method A calls function/method B
- imports      — file A imports file/package B
- defines      — file/package A defines symbol B
- implements   — type A implements interface B (structural inference)
- extends      — class A extends class B
- references   — symbol A references type/variable B
- member_of    — method/field A belongs to type B
- instantiates — function A creates instance of type B
- similar_to   — function/method A is a near-duplicate (clone) of B
- package_workspace_member — package-manager workspace root A owns member package B
- cross_repo_calls      — calls edge whose target lives in another repo
- cross_repo_implements — implements edge crossing a repo boundary
- cross_repo_extends    — extends edge crossing a repo boundary

## Node ID Format
  file_path::SymbolName
  file_path::TypeName.MethodName

## Meta Fields
- signature  — function/method signature string
- receiver   — method receiver type name
- methods    — interface/trait method names ([]string, for IMPLEMENTS inference)
- proto_type — protobuf: "message", "enum"
- sql_type   — SQL: "table", "view", "index", "trigger"
- visibility — "private" for unexported symbols
`
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "text/plain",
			Text:     schema,
		},
	}, nil
}

func (s *Server) handleResourceCommunities(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	comms := s.getCommunities()
	if comms == nil || len(comms.Communities) == 0 {
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     `{"communities":[],"message":"no communities detected yet"}`,
			},
		}, nil
	}

	type summary struct {
		ID       string   `json:"id"`
		Label    string   `json:"label"`
		Size     int      `json:"size"`
		Files    []string `json:"files"`
		Cohesion float64  `json:"cohesion"`
	}
	var summaries []summary
	for _, c := range comms.Communities {
		summaries = append(summaries, summary{
			ID: c.ID, Label: c.Label, Size: c.Size,
			Files: c.Files, Cohesion: c.Cohesion,
		})
	}

	data, err := json.Marshal(map[string]any{
		"communities": summaries,
		"total":       len(summaries),
		"modularity":  comms.Modularity,
	})
	if err != nil {
		return nil, err
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (s *Server) handleResourceCommunity(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	comms := s.getCommunities()
	if comms == nil {
		return nil, fmt.Errorf("no communities detected yet")
	}

	// Extract ID from URI: gortex://community/{id}
	id := extractURIParam(req.Params.URI, "gortex://community/")
	if id == "" {
		return nil, fmt.Errorf("missing community id in URI")
	}

	for _, c := range comms.Communities {
		if c.ID == id {
			data, err := json.Marshal(c)
			if err != nil {
				return nil, err
			}
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      req.Params.URI,
					MIMEType: "application/json",
					Text:     string(data),
				},
			}, nil
		}
	}
	return nil, fmt.Errorf("community not found: %s", id)
}

func (s *Server) handleResourceProcesses(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	procs := s.getProcesses()
	if procs == nil || len(procs.Processes) == 0 {
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     `{"processes":[],"message":"no processes discovered yet"}`,
			},
		}, nil
	}

	type summary struct {
		ID         string  `json:"id"`
		Name       string  `json:"name"`
		EntryPoint string  `json:"entry_point"`
		StepCount  int     `json:"step_count"`
		FileCount  int     `json:"file_count"`
		Score      float64 `json:"score"`
	}
	var summaries []summary
	for _, p := range procs.Processes {
		summaries = append(summaries, summary{
			ID: p.ID, Name: p.Name, EntryPoint: p.EntryPoint,
			StepCount: p.StepCount, FileCount: len(p.Files), Score: p.Score,
		})
	}

	data, err := json.Marshal(map[string]any{
		"processes": summaries,
		"total":     len(summaries),
	})
	if err != nil {
		return nil, err
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (s *Server) handleResourceProcess(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	procs := s.getProcesses()
	if procs == nil {
		return nil, fmt.Errorf("no processes discovered yet")
	}

	id := extractURIParam(req.Params.URI, "gortex://process/")
	if id == "" {
		return nil, fmt.Errorf("missing process id in URI")
	}

	for _, p := range procs.Processes {
		if p.ID == id {
			data, err := json.Marshal(p)
			if err != nil {
				return nil, err
			}
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      req.Params.URI,
					MIMEType: "application/json",
					Text:     string(data),
				},
			}, nil
		}
	}
	return nil, fmt.Errorf("process not found: %s", id)
}

// extractURIParam extracts the parameter value after a URI prefix.
// e.g. extractURIParam("gortex://community/community-0", "gortex://community/") => "community-0"
func extractURIParam(uri, prefix string) string {
	if len(uri) > len(prefix) && uri[:len(prefix)] == prefix {
		return uri[len(prefix):]
	}
	return ""
}

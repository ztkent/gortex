package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/llm"
)

// registerLLMTools registers the `ask` MCP tool when an LLM service
// has been attached via SetLLMService and the service is enabled
// (model path configured). When either is missing, the tool is
// silently absent from tools/list — clean degradation for builds /
// deployments without an LLM.
func (s *Server) registerLLMTools() {
	if s.llmService == nil || !s.llmService.Enabled() {
		return
	}
	s.mcpServer.AddTool(
		mcp.NewTool("ask",
			mcp.WithDescription("Ask a research agent to navigate the gortex graph and return a synthesized answer. The agent runs on whichever LLM provider is configured (`llm.provider`): an in-process llama.cpp model, or a hosted Anthropic / OpenAI / Ollama backend. Use this instead of issuing many search_symbols / get_callers / contracts calls yourself when the question is open-ended or requires multi-hop reasoning across repos — the agent does that work and returns a filtered answer. Set chain=true for cross-system call-chain tracing (consumer → contract → provider → downstream)."),
			mcp.WithString("question", mcp.Required(), mcp.Description("Natural-language question about the indexed codebase. Examples: \"who calls NewServer in the mcp package?\", \"trace the path from web's /v1/stats consumer to the gortex handler\".")),
			mcp.WithString("repo", mcp.Description("Optional repo-prefix scope (e.g. \"gortex-cloud\"). Restricts the agent's tool calls to one repo. Leave empty for cross-repo questions.")),
			mcp.WithString("project", mcp.Description("Optional project scope.")),
			mcp.WithString("ref", mcp.Description("Optional ref tag scope.")),
			mcp.WithBoolean("chain", mcp.Description("Enable cross-system chain mode: gives the agent the contracts + get_dependencies tools and a chain-tracing prompt. Use when the question is about how a request flows across repos. Default false.")),
			mcp.WithBoolean("include_transcript", mcp.Description("Include the agent's full step-by-step transcript in the response. Useful for debugging the agent's reasoning. Default false (compact response).")),
		),
		s.handleAsk,
	)
}

// handleAsk delegates a single MCP `ask` invocation to the LLM
// service's RunAgent. The agent's typed AgentAnswer is JSON-marshaled
// into the MCP text content block — that's the same shape the
// existing handlers use for structured responses.
func (s *Server) handleAsk(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.llmService == nil || !s.llmService.Enabled() {
		return mcp.NewToolResultError("llm: service is not configured on this server"), nil
	}
	args := req.GetArguments()
	question, _ := args["question"].(string)
	repo, _ := args["repo"].(string)
	project, _ := args["project"].(string)
	ref, _ := args["ref"].(string)
	chain, _ := boolArg(args, "chain")
	includeTranscript, _ := boolArg(args, "include_transcript")

	answer, err := s.llmService.RunAgent(ctx, llm.RunAgentOptions{
		Question:          question,
		Scope:             llm.Scope{Repo: repo, Project: project, Ref: ref},
		Chain:             chain,
		IncludeTranscript: includeTranscript,
	})
	if err != nil && answer == nil {
		return mcp.NewToolResultError(fmt.Sprintf("llm: %v", err)), nil
	}
	out, mErr := json.MarshalIndent(answer, "", "  ")
	if mErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("llm: marshal answer: %v", mErr)), nil
	}
	return mcp.NewToolResultText(string(out)), nil
}

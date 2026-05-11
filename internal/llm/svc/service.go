//go:build llama

// Package svc is the runner layer that ties the LLM model (package
// llm) to the agent loop (package llm/agent). It lives in its own
// package to break the import cycle that would otherwise exist
// between `llm` (defines Context, Backend) and `llm/agent` (depends
// on those types).
package svc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/agent"
)

// Service is the reusable in-process LLM access point. Wraps a
// lazily-loaded llama.cpp model plus a Backend (typically an
// InProcessBackend pointing at the daemon's *query.Engine). Two
// consumption shapes:
//
//   - Generate: one-shot prompt → text. Used by future wiki / doc
//     generation features that don't need a tool-calling loop.
//   - RunAgent: grammar-constrained agent loop that uses Backend's
//     tools to navigate the graph and produce a synthesized answer.
//     Used by the MCP `ask` tool handler.
//
// Both go through the same model and the same inference mutex —
// llama.cpp is single-stream on a given device.
type Service struct {
	cfg     llm.Config
	backend llm.Backend

	loadOnce sync.Once
	model    *llm.Model
	loadErr  error

	infer sync.Mutex
}

// NewService is cheap — it just stores the config and backend. The
// model is mmap'd and Metal kernels compiled lazily on the first
// Generate / RunAgent call, so daemon startup isn't slowed.
func NewService(cfg llm.Config, backend llm.Backend) *Service {
	return &Service{
		cfg:     cfg.ApplyDefaults(),
		backend: backend,
	}
}

// Enabled reports whether the service has a valid configuration
// (non-empty model path) and a backend. Callers should check this
// before registering features that depend on the service.
func (s *Service) Enabled() bool {
	return s != nil && s.cfg.IsEnabled() && s.backend != nil
}

func (s *Service) ensureLoaded() error {
	s.loadOnce.Do(func() {
		if !s.cfg.IsEnabled() {
			s.loadErr = errors.New("llm: model path is empty")
			return
		}
		m, err := llm.LoadModel(s.cfg.Model, s.cfg.GPULayers)
		if err != nil {
			s.loadErr = fmt.Errorf("llm: load model: %w", err)
			return
		}
		s.model = m
	})
	return s.loadErr
}

// Close releases the underlying model. Safe to call multiple times.
// After Close, every operational method returns an error.
func (s *Service) Close() error {
	s.infer.Lock()
	defer s.infer.Unlock()
	if s.model != nil {
		s.model.Close()
		s.model = nil
	}
	return nil
}

// Generate runs one-shot inference: prompt in, generated text out.
// No agent loop, no tools — just the model. Intended for future
// summarization / wiki generation use cases where the caller assembles
// the prompt with relevant code context itself.
//
// maxTokens caps the generation length; 0 falls back to a sensible
// default (1024). The model's chat template is NOT applied — pass a
// fully-formatted prompt.
func (s *Service) Generate(ctx context.Context, prompt string, maxTokens int) (string, error) {
	_ = ctx // greedy inference is uninterruptible in the current wrapper
	if err := s.ensureLoaded(); err != nil {
		return "", err
	}
	if maxTokens <= 0 {
		maxTokens = 1024
	}

	s.infer.Lock()
	defer s.infer.Unlock()

	llmCtx, err := s.model.NewContext(s.cfg.Ctx, 0)
	if err != nil {
		return "", fmt.Errorf("llm: new context: %w", err)
	}
	defer llmCtx.Close()

	var out strings.Builder
	_, err = llmCtx.Generate(prompt, maxTokens, func(piece string) bool {
		out.WriteString(piece)
		return true
	})
	if err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

// RunAgent runs the grammar-constrained tool-calling agent loop. The
// agent issues tool calls against the configured Backend (typically an
// InProcessBackend wired to gortex's *query.Engine) and synthesizes a
// final answer via the model's final_answer tool.
//
// Returned AgentAnswer always has at least Answer/Error populated —
// non-nil even on error paths.
func (s *Service) RunAgent(ctx context.Context, opts llm.RunAgentOptions) (*llm.AgentAnswer, error) {
	_ = ctx
	answer := &llm.AgentAnswer{Scope: opts.Scope, ChainMode: opts.Chain}
	if err := s.ensureLoaded(); err != nil {
		answer.Error = err.Error()
		return answer, err
	}
	if strings.TrimSpace(opts.Question) == "" {
		err := errors.New("llm: question is empty")
		answer.Error = err.Error()
		return answer, err
	}

	tmpl, err := agent.TemplateByName(s.cfg.Template)
	if err != nil {
		answer.Error = err.Error()
		return answer, err
	}

	systemExtras := opts.SystemExtras
	if systemExtras == "" {
		if opts.Chain {
			systemExtras = promptChain
		} else {
			systemExtras = promptSimple
		}
	}

	var tools []agent.Tool
	if opts.Chain {
		tools = agent.GortexChainTools(s.backend, opts.Scope)
	} else {
		tools = agent.GortexTools(s.backend, opts.Scope)
	}

	s.infer.Lock()
	defer s.infer.Unlock()

	llmCtx, err := s.model.NewContext(s.cfg.Ctx, 0)
	if err != nil {
		answer.Error = err.Error()
		return answer, err
	}
	defer llmCtx.Close()

	ag, err := agent.New(llmCtx, tools, tmpl)
	if err != nil {
		answer.Error = err.Error()
		return answer, err
	}

	t0 := time.Now()
	answerText, transcript, runErr := ag.Run(systemExtras, opts.Question, s.cfg.MaxSteps)
	answer.ElapsedMs = time.Since(t0).Milliseconds()
	answer.Answer = answerText

	steps := 0
	for _, st := range transcript {
		if st.Kind == "call" || st.Kind == "final" {
			steps++
		}
	}
	answer.Steps = steps

	if opts.IncludeTranscript {
		answer.Transcript = make([]llm.TranscriptStep, 0, len(transcript))
		for _, st := range transcript {
			answer.Transcript = append(answer.Transcript, llm.TranscriptStep{
				Kind: st.Kind, Raw: st.Raw, Tool: st.Tool,
			})
		}
	}
	if runErr != nil {
		answer.Error = runErr.Error()
	}
	return answer, runErr
}

// promptSimple — P2-equivalent rules from the bench experiments. Tight
// system prompt for single-hop / cross-repo lookups.
const promptSimple = `RULES (follow these exactly):
- If the user gives you only a bare name (not a path-qualified id like "pkg/x.Foo"), you MUST first call search_symbols to resolve it to an id before calling get_callers.
- For search_symbols, pass ONLY the bare symbol name as "query" — no prepositions, no package qualifiers, no extra words.
- search_symbols returns ranked matches; the FIRST few are best. Pick at most the top 1-3 that look like functions or methods.
- Make at least one real tool call before final_answer.
- Never call the same tool with the same args twice in a row.
- When you have enough information, call final_answer summarising what you found.`

// promptChain — chain-mode rules with the explicit "no get_callers"
// direction warning that we proved closes Coder-7B's directional
// confusion in the bench.
const promptChain = `RULES (follow these exactly):
- You are tracing a cross-system call chain. Output one tool call per turn.
- DIRECTION MATTERS. Only these tools are correct in chain mode:
    * contracts        — find producer↔consumer pairs across repos
    * get_dependencies — FORWARD direction: what does this symbol call/import?
    * final_answer     — emit the chain
  Do NOT use get_callers. get_callers walks BACKWARDS (who calls X), which is
  the WRONG direction for chain tracing and will lead you astray.
- For search_symbols and contracts, pass clean values (no extra words).
- Typical flow for "trace request X across systems":
  1) contracts({"role":"consumer","path":"<path>"}) — find the caller side.
  2) contracts({"role":"provider","path":"<path>"}) — find the handler.
  3) get_dependencies({"id":"<provider symbol_id>"}) — see what the handler calls.
  4) For deeper hops, call get_dependencies AGAIN on the most interesting result's id.
  5) Look for deps whose repo prefix differs from the handler's repo —
     those are the cross-repo downstream calls.
  6) Call final_answer with the chain as numbered steps.
- Never call the same tool with the same args twice in a row.
- final_answer.text should list each system hop with its symbol id and repo.`

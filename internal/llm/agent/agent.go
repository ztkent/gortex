// Package agent runs a provider-agnostic tool-calling loop. On each
// turn the model emits one JSON object {"tool":"<name>","args":{...}};
// the loop executes that call and feeds the result back as a new turn.
// The loop terminates when the model calls the final_answer tool.
//
// The structured-output constraint — the model may only emit a valid
// tool-call object — is enforced by the llm.Provider via
// CompletionRequest.Shape == ShapeToolCall: a GBNF grammar for the
// local llama.cpp provider, json-schema / forced-tool for the HTTP
// providers, and a JSON-Schema rider injected into the system prompt
// plus a robust JSON extractor for the claudecli subprocess provider.
// The loop carries the conversation as a provider-neutral
// []llm.Message, so the same agent drives every provider.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/llm"
)

// ToolFunc executes a single tool call. args is the parsed JSON object
// the model emitted under "args". The returned string is fed back to
// the model verbatim as the tool's observation.
type ToolFunc func(args map[string]any) (string, error)

type Tool struct {
	Name        string
	Description string
	Run         ToolFunc
}

// Step is one entry in the conversation transcript. Kind is "call",
// "result", or "final".
type Step struct {
	Kind string
	Raw  string         // raw JSON for calls; raw result for "result"; answer for "final"
	Tool string         // name of tool invoked (call/final)
	Args map[string]any // parsed args (call/final)
}

// FinalAnswerTool is the name of the synthetic terminator tool. It is
// registered automatically by New; callers must not pre-register it.
const FinalAnswerTool = "final_answer"

// stepMaxTokens caps a single tool-call emission. A tool call is a
// small JSON object, so this is generous.
const stepMaxTokens = 512

type Agent struct {
	provider llm.Provider
	tools    map[string]Tool
	names    []string       // sorted, stable iteration order
	specs    []llm.ToolSpec // sorted by name; handed to the provider

	// compactor bounds a long multi-turn conversation by folding older
	// rounds into a rolling summary. Nil (the default) disables compaction
	// entirely — Run then behaves exactly as it did before the compactor
	// existed. Installed via WithCompactor.
	compactor *RollingCompactor

	// lastUsage is the summed token usage of the most recent Run — the
	// per-step provider usage accumulated across the tool-calling loop,
	// plus any tokens the rolling-summary compactor spent. Zero for a
	// provider that does not report usage. Read via LastUsage.
	//
	// usageMu guards lastUsage because the compactor may attribute a
	// background summarizer's usage from a separate goroutine.
	usageMu   sync.Mutex
	lastUsage llm.TokenUsage
}

// New builds an Agent over a provider and a tool set. The synthetic
// final_answer tool is appended automatically. Returns an error for a
// nil provider or a malformed tool (empty name, reserved name, nil
// Run). Optional AgentOptions (e.g. WithCompactor) tune behaviour; an
// Agent built with no options behaves exactly as before.
func New(provider llm.Provider, tools []Tool, opts ...AgentOption) (*Agent, error) {
	if provider == nil {
		return nil, errors.New("agent: nil provider")
	}
	reg := make(map[string]Tool, len(tools)+1)
	for _, t := range tools {
		if t.Name == "" {
			return nil, errors.New("agent: tool has empty name")
		}
		if t.Name == FinalAnswerTool {
			return nil, fmt.Errorf("agent: %q is reserved", FinalAnswerTool)
		}
		if t.Run == nil {
			return nil, fmt.Errorf("agent: tool %q has nil Run", t.Name)
		}
		reg[t.Name] = t
	}
	reg[FinalAnswerTool] = Tool{
		Name:        FinalAnswerTool,
		Description: `Emit the final answer to the user and terminate. args: {"text": "<answer>"}.`,
		Run:         nil, // handled inline by Run()
	}

	names := make([]string, 0, len(reg))
	for n := range reg {
		names = append(names, n)
	}
	sort.Strings(names)
	specs := make([]llm.ToolSpec, len(names))
	for i, n := range names {
		specs[i] = llm.ToolSpec{Name: n, Description: reg[n].Description}
	}

	a := &Agent{provider: provider, tools: reg, names: names, specs: specs}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a, nil
}

// Run executes the tool-calling loop until the model invokes
// final_answer or maxSteps is reached. The transcript captures every
// call/result/final step in order.
func (a *Agent) Run(ctx context.Context, systemExtras, userQuestion string, maxSteps int) (answer string, transcript []Step, err error) {
	// runCtx is the parent of any background summarizer the compactor
	// spawns; the deferred cancel guarantees no summarizer outlives this
	// Run (and a summary that lands after cancellation is dropped — see
	// RollingCompactor.maybeCompact). The subsequent wait drains the
	// goroutine so neither it nor its usage attribution leaks past Run.
	runCtx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		if a.compactor != nil {
			a.compactor.wait()
		}
	}()

	conv := []llm.Message{
		{Role: llm.RoleSystem, Content: a.systemPrompt(systemExtras)},
		{Role: llm.RoleUser, Content: userQuestion},
	}
	seen := map[string]struct{}{}
	a.usageMu.Lock()
	a.lastUsage = llm.TokenUsage{}
	a.usageMu.Unlock()

	for step := range maxSteps {
		resp, gerr := a.provider.Complete(runCtx, llm.CompletionRequest{
			Messages:  conv,
			MaxTokens: stepMaxTokens,
			Shape:     llm.ShapeToolCall,
			Tools:     a.specs,
		})
		if gerr != nil {
			return "", transcript, fmt.Errorf("step %d generate: %w", step, gerr)
		}
		a.usageMu.Lock()
		a.lastUsage.Add(resp.Usage)
		stepUsage := a.lastUsage
		a.usageMu.Unlock()

		raw := strings.TrimSpace(resp.Text)
		call, perr := parseToolCall(raw)
		if perr != nil {
			return "", transcript, fmt.Errorf("step %d parse %q: %w", step, raw, perr)
		}

		if call.Tool == FinalAnswerTool {
			text, _ := call.Args["text"].(string)
			transcript = append(transcript, Step{
				Kind: "final", Raw: text, Tool: call.Tool, Args: call.Args,
			})
			return text, transcript, nil
		}

		tool, ok := a.tools[call.Tool]
		if !ok {
			// Structured output shouldn't allow this, but defend anyway.
			return "", transcript, fmt.Errorf("step %d unknown tool %q (provider bug?)", step, call.Tool)
		}
		transcript = append(transcript, Step{
			Kind: "call", Raw: raw, Tool: call.Tool, Args: call.Args,
		})

		// Loop detection: if we've already executed this exact
		// (tool, args) pair in this run, refuse to execute it again and
		// feed back a synthetic loop_detected observation so the model
		// is forced to change strategy.
		key := callKey(call.Tool, call.Args)
		if _, dup := seen[key]; dup {
			loopResult := `{"error":"loop_detected","message":"You already called this exact tool with these exact args; the result did not help. Try DIFFERENT args, a DIFFERENT tool, or call final_answer to give your best summary of what you found."}`
			transcript = append(transcript, Step{Kind: "result", Raw: loopResult})
			conv = append(conv,
				llm.Message{Role: llm.RoleAssistant, Content: raw},
				llm.Message{Role: llm.RoleTool, Content: loopResult, ToolName: call.Tool},
			)
			conv = a.compactConversation(runCtx, conv, stepUsage)
			continue
		}
		seen[key] = struct{}{}

		result, terr := tool.Run(call.Args)
		if terr != nil {
			result = fmt.Sprintf(`{"error": %q}`, terr.Error())
		}
		transcript = append(transcript, Step{Kind: "result", Raw: result})

		conv = append(conv,
			llm.Message{Role: llm.RoleAssistant, Content: raw},
			llm.Message{Role: llm.RoleTool, Content: result, ToolName: call.Tool},
		)
		conv = a.compactConversation(runCtx, conv, stepUsage)
	}
	return "", transcript, fmt.Errorf("agent: exceeded %d steps without final_answer", maxSteps)
}

// compactConversation runs the rolling-summary compactor over conv after a
// round has been appended. It is a no-op when no compactor is installed (or
// the conversation is still under the high-water mark), so short loops are
// untouched. The compactor attributes any summarizer tokens it spends into
// a.lastUsage under a.usageMu.
func (a *Agent) compactConversation(runCtx context.Context, conv []llm.Message, stepUsage llm.TokenUsage) []llm.Message {
	if a.compactor == nil || !a.compactor.enabled() {
		return conv
	}
	size := estimateConvTokens(conv, stepUsage)
	compacted, _ := a.compactor.maybeCompact(runCtx, conv, size, &a.lastUsage, &a.usageMu)
	return compacted
}

// LastUsage returns the token usage summed across the steps of the most
// recent Run, including any tokens the rolling-summary compactor spent.
// Zero before the first Run, or when the provider does not report usage
// (subprocess / not-yet-decoded HTTP providers).
func (a *Agent) LastUsage() llm.TokenUsage {
	if a == nil {
		return llm.TokenUsage{}
	}
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	return a.lastUsage
}

// systemPrompt assembles the agent's system message: the tool-call
// protocol, the tool catalogue, and any caller-supplied extras
// (the simple / chain mode rule sets).
func (a *Agent) systemPrompt(extras string) string {
	var sys strings.Builder
	sys.WriteString("You are a tool-using agent. ")
	sys.WriteString("On each turn, emit ONE JSON object: ")
	sys.WriteString(`{"tool": "<name>", "args": {...}}.`)
	sys.WriteString(" Available tools:\n")
	for _, n := range a.names {
		fmt.Fprintf(&sys, "- %s: %s\n", n, a.tools[n].Description)
	}
	sys.WriteString("\nAfter receiving a tool result, call the next tool. ")
	sys.WriteString("When you have enough information, call ")
	sys.WriteString(FinalAnswerTool)
	sys.WriteString(` with the final answer text.`)
	if extras != "" {
		sys.WriteString("\n\n")
		sys.WriteString(extras)
	}
	return sys.String()
}

type toolCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

func parseToolCall(s string) (toolCall, error) {
	var c toolCall
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return c, err
	}
	if c.Tool == "" {
		return c, errors.New(`missing "tool"`)
	}
	if c.Args == nil {
		c.Args = map[string]any{}
	}
	return c, nil
}

// callKey canonicalises a (tool, args) pair for loop detection.
// json.Marshal on a map[string]any sorts keys, giving a stable form.
func callKey(tool string, args map[string]any) string {
	b, _ := json.Marshal(args)
	return tool + ":" + string(b)
}

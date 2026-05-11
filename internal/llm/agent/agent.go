//go:build llama

// Package agent runs a grammar-constrained tool-calling loop on top of
// the internal/llm wrapper. The model can only emit JSON of the
// shape {"tool": "<one of the registered names>", "args": {...}};
// each tool call is executed and its result fed back as a new turn.
// The loop terminates when the model calls the final_answer tool.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	llm "github.com/zzet/gortex/internal/llm"
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

// ChatTemplate describes how to wrap conversation turns for a given
// model family. The four wrappers each take raw content and return
// the fully-marked-up turn; AssistPrime is the marker we append to
// the running conversation right before each generate call so the
// model starts emitting an assistant turn.
type ChatTemplate struct {
	Name        string
	BOS         string
	System      func(content string) string
	User        func(content string) string
	AssistEnd   string // marker appended after a captured assistant emission
	Tool        func(content string) string
	AssistPrime string
}

// TemplateChatML covers Qwen2.5 family and Nous Hermes-3 (which
// re-trains Llama-3 onto ChatML).
var TemplateChatML = ChatTemplate{
	Name:      "chatml",
	System:    func(c string) string { return "<|im_start|>system\n" + c + "<|im_end|>\n" },
	User:      func(c string) string { return "<|im_start|>user\n" + c + "<|im_end|>\n" },
	AssistEnd: "<|im_end|>\n",
	Tool:      func(c string) string { return "<|im_start|>tool\n" + c + "<|im_end|>\n" },
	AssistPrime: "<|im_start|>assistant\n",
}

// TemplateLlama3 covers Meta's Llama-3.x stock instruct format. Used by
// models that keep Llama-3's native template (NOT Hermes-3, which
// switches to ChatML).
var TemplateLlama3 = ChatTemplate{
	Name: "llama3",
	BOS:  "<|begin_of_text|>",
	System: func(c string) string {
		return "<|start_header_id|>system<|end_header_id|>\n\n" + c + "<|eot_id|>"
	},
	User: func(c string) string {
		return "<|start_header_id|>user<|end_header_id|>\n\n" + c + "<|eot_id|>"
	},
	AssistEnd: "<|eot_id|>",
	Tool: func(c string) string {
		return "<|start_header_id|>ipython<|end_header_id|>\n\n" + c + "<|eot_id|>"
	},
	AssistPrime: "<|start_header_id|>assistant<|end_header_id|>\n\n",
}

// TemplateByName returns a known chat template by short name.
func TemplateByName(name string) (ChatTemplate, error) {
	switch name {
	case "", "chatml", "qwen", "hermes":
		return TemplateChatML, nil
	case "llama3", "llama":
		return TemplateLlama3, nil
	}
	return ChatTemplate{}, fmt.Errorf("unknown chat template %q", name)
}

type Agent struct {
	ctx     *llm.Context
	tmpl    ChatTemplate
	tools   map[string]Tool
	names   []string // sorted, for stable grammar
	grammar string
	maxTok  int
}

// FinalAnswerTool is the name of the synthetic terminator tool. It is
// registered automatically by New; callers should not pre-register it.
const FinalAnswerTool = "final_answer"

func New(ctx *llm.Context, tools []Tool, tmpl ChatTemplate) (*Agent, error) {
	if tmpl.AssistPrime == "" {
		tmpl = TemplateChatML
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

	a := &Agent{
		ctx:    ctx,
		tmpl:   tmpl,
		tools:  reg,
		names:  names,
		maxTok: 512,
	}
	a.grammar = buildGrammar(names)
	if err := ctx.SetGrammar(a.grammar); err != nil {
		return nil, fmt.Errorf("agent: install grammar: %w", err)
	}
	return a, nil
}

// Grammar returns the GBNF the agent installed. Exposed for debugging.
func (a *Agent) Grammar() string { return a.grammar }

// Run executes the tool-calling loop until the model invokes
// final_answer or maxSteps is reached. The transcript captures every
// call/result/final step in order.
func (a *Agent) Run(systemExtras, userQuestion string, maxSteps int) (answer string, transcript []Step, err error) {
	conv := a.initialPrompt(systemExtras, userQuestion)
	seen := map[string]struct{}{}

	for step := 0; step < maxSteps; step++ {
		a.ctx.Reset()

		var buf strings.Builder
		_, gerr := a.ctx.Generate(conv, a.maxTok, func(piece string) bool {
			buf.WriteString(piece)
			return !jsonComplete(buf.String())
		})
		if gerr != nil {
			return "", transcript, fmt.Errorf("step %d generate: %w", step, gerr)
		}

		raw := strings.TrimSpace(buf.String())
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
			// Grammar shouldn't allow this, but defend anyway.
			return "", transcript, fmt.Errorf("step %d unknown tool %q (grammar bug?)", step, call.Tool)
		}
		transcript = append(transcript, Step{
			Kind: "call", Raw: raw, Tool: call.Tool, Args: call.Args,
		})

		// Loop detection: if we've already executed this exact
		// (tool, args) pair in this run, refuse to execute it again
		// and feed back a synthetic loop_detected observation so the
		// model is forced to change strategy.
		key := callKey(call.Tool, call.Args)
		if _, dup := seen[key]; dup {
			loopResult := `{"error":"loop_detected","message":"You already called this exact tool with these exact args; the result did not help. Try DIFFERENT args, a DIFFERENT tool, or call final_answer to give your best summary of what you found."}`
			transcript = append(transcript, Step{Kind: "result", Raw: loopResult})
			conv += raw + a.tmpl.AssistEnd +
				a.tmpl.Tool(loopResult) +
				a.tmpl.AssistPrime
			continue
		}
		seen[key] = struct{}{}

		result, terr := tool.Run(call.Args)
		if terr != nil {
			result = fmt.Sprintf(`{"error": %q}`, terr.Error())
		}
		transcript = append(transcript, Step{Kind: "result", Raw: result})

		conv += raw + a.tmpl.AssistEnd +
			a.tmpl.Tool(result) +
			a.tmpl.AssistPrime
	}
	return "", transcript, fmt.Errorf("agent: exceeded %d steps without final_answer", maxSteps)
}

func (a *Agent) initialPrompt(extras, question string) string {
	var sys strings.Builder
	sys.WriteString("You are a tool-using agent. ")
	sys.WriteString("On each turn, emit ONE JSON object: ")
	sys.WriteString(`{"tool": "<name>", "args": {...}}.`)
	sys.WriteString(" Available tools:\n")
	for _, n := range a.names {
		t := a.tools[n]
		fmt.Fprintf(&sys, "- %s: %s\n", n, t.Description)
	}
	sys.WriteString("\nAfter receiving a tool result, call the next tool. ")
	sys.WriteString("When you have enough information, call ")
	sys.WriteString(FinalAnswerTool)
	sys.WriteString(` with the final answer text.`)
	if extras != "" {
		sys.WriteString("\n\n")
		sys.WriteString(extras)
	}

	return a.tmpl.BOS +
		a.tmpl.System(sys.String()) +
		a.tmpl.User(question) +
		a.tmpl.AssistPrime
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

// jsonComplete reports whether s is a complete top-level JSON object.
// Used to early-stop generation as soon as the grammar-driven model
// closes the brace, instead of waiting on EOS.
func jsonComplete(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return false
	}
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}

// buildGrammar returns a GBNF that accepts {"tool": "<one of names>",
// "args": {<arbitrary JSON object>}} with whitespace tolerance.
func buildGrammar(names []string) string {
	alt := make([]string, len(names))
	for i, n := range names {
		alt[i] = `"\"" "` + n + `" "\""`
	}
	toolname := strings.Join(alt, " | ")

	return `root ::= ws "{" ws "\"tool\"" ws ":" ws toolname ws "," ws "\"args\"" ws ":" ws object ws "}" ws
toolname ::= ` + toolname + `
object ::= "{" ws ( pair ( ws "," ws pair )* )? ws "}"
pair ::= string ws ":" ws value
array ::= "[" ws ( value ( ws "," ws value )* )? ws "]"
value ::= string | number | object | array | "true" | "false" | "null"
string ::= "\"" ( [^"\\] | "\\" ( ["\\/bfnrt] | "u" [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] ) )* "\""
number ::= "-"? ( "0" | [1-9] [0-9]* ) ( "." [0-9]+ )? ( [eE] [-+]? [0-9]+ )?
ws ::= [ \t\n]*
`
}

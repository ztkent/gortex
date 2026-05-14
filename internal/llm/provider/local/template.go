//go:build llama

// Package local — chat templates and GBNF grammars.
//
// These are the parts of the old internal/llm/agent and
// internal/llm/svc layers that are specific to a local llama.cpp
// model: how to flatten a provider-neutral []llm.Message into a single
// prompt string with the right turn markers, and how to constrain
// token sampling to a JSON shape via GBNF. The HTTP providers need
// neither — they speak structured messages and json-schema natively.
package local

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/llm"
)

// chatTemplate describes how to wrap conversation turns for a given
// model family. The wrappers each take raw content and return the
// fully-marked-up turn; assistPrime is the marker appended right
// before a generate call so the model starts an assistant turn.
type chatTemplate struct {
	name        string
	bos         string
	system      func(content string) string
	user        func(content string) string
	tool        func(content string) string
	assistEnd   string // marker appended after a captured assistant emission
	assistPrime string
}

// templateChatML covers the Qwen2.5 family and Nous Hermes-3 (which
// re-trains Llama-3 onto ChatML).
var templateChatML = chatTemplate{
	name:        "chatml",
	system:      func(c string) string { return "<|im_start|>system\n" + c + "<|im_end|>\n" },
	user:        func(c string) string { return "<|im_start|>user\n" + c + "<|im_end|>\n" },
	tool:        func(c string) string { return "<|im_start|>tool\n" + c + "<|im_end|>\n" },
	assistEnd:   "<|im_end|>\n",
	assistPrime: "<|im_start|>assistant\n",
}

// templateLlama3 covers Meta's Llama-3.x stock instruct format. Used
// by models that keep Llama-3's native template (NOT Hermes-3, which
// switches to ChatML).
var templateLlama3 = chatTemplate{
	name: "llama3",
	bos:  "<|begin_of_text|>",
	system: func(c string) string {
		return "<|start_header_id|>system<|end_header_id|>\n\n" + c + "<|eot_id|>"
	},
	user: func(c string) string {
		return "<|start_header_id|>user<|end_header_id|>\n\n" + c + "<|eot_id|>"
	},
	tool: func(c string) string {
		return "<|start_header_id|>ipython<|end_header_id|>\n\n" + c + "<|eot_id|>"
	},
	assistEnd:   "<|eot_id|>",
	assistPrime: "<|start_header_id|>assistant<|end_header_id|>\n\n",
}

// templateByName returns a known chat template by short name. Empty
// falls back to ChatML.
func templateByName(name string) (chatTemplate, error) {
	switch name {
	case "", "chatml", "qwen", "hermes":
		return templateChatML, nil
	case "llama3", "llama":
		return templateLlama3, nil
	}
	return chatTemplate{}, fmt.Errorf("local: unknown chat template %q", name)
}

// flatten renders a provider-neutral conversation into a single prompt
// string and primes an assistant turn. RoleAssistant messages are
// wrapped as a complete assistant emission (prime + content + end);
// RoleTool messages use the tool wrapper. The trailing assistPrime is
// what makes the model start generating an assistant turn.
func (t chatTemplate) flatten(msgs []llm.Message) string {
	var b strings.Builder
	b.WriteString(t.bos)
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			b.WriteString(t.system(m.Content))
		case llm.RoleUser:
			b.WriteString(t.user(m.Content))
		case llm.RoleAssistant:
			b.WriteString(t.assistPrime)
			b.WriteString(m.Content)
			b.WriteString(t.assistEnd)
		case llm.RoleTool:
			b.WriteString(t.tool(m.Content))
		}
	}
	b.WriteString(t.assistPrime)
	return b.String()
}

// --- GBNF grammars ----------------------------------------------------------

// The expand / rerank / verify grammars each accept a single-key JSON
// object whose value is a string array. The array body is fully
// optional so the model is always allowed to emit an empty list —
// for verify that empty list is the load-bearing "honest negative".
const (
	expandGrammar = `root ::= ws "{" ws "\"terms\"" ws ":" ws "[" ws ( str ( ws "," ws str )* )? ws "]" ws "}" ws
str ::= "\"" ( [^"\\] | "\\" ( ["\\/bfnrt] | "u" [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] ) )* "\""
ws ::= [ \t\n]*
`
	rerankGrammar = `root ::= ws "{" ws "\"order\"" ws ":" ws "[" ws ( str ( ws "," ws str )* )? ws "]" ws "}" ws
str ::= "\"" ( [^"\\] | "\\" ( ["\\/bfnrt] | "u" [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] ) )* "\""
ws ::= [ \t\n]*
`
	verifyGrammar = `root ::= ws "{" ws "\"keep\"" ws ":" ws "[" ws ( str ( ws "," ws str )* )? ws "]" ws "}" ws
str ::= "\"" ( [^"\\] | "\\" ( ["\\/bfnrt] | "u" [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] ) )* "\""
ws ::= [ \t\n]*
`
)

// buildToolCallGrammar returns a GBNF that accepts
// {"tool":"<one of names>","args":{<arbitrary JSON object>}} with
// whitespace tolerance. names must be non-empty.
func buildToolCallGrammar(names []string) string {
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

// grammarForShape returns the GBNF for a structured shape. ShapeFreeform
// returns "" (no constraint). ShapeToolCall requires the tool name set.
func grammarForShape(shape llm.JSONShape, tools []llm.ToolSpec) string {
	switch shape {
	case llm.ShapeExpandTerms:
		return expandGrammar
	case llm.ShapeRerankOrder:
		return rerankGrammar
	case llm.ShapeVerifyKeep:
		return verifyGrammar
	case llm.ShapeToolCall:
		names := make([]string, len(tools))
		for i, t := range tools {
			names[i] = t.Name
		}
		return buildToolCallGrammar(names)
	default:
		return ""
	}
}

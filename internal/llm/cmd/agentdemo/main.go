//go:build llama

// agentdemo: drive the grammar-constrained tool-calling agent against
// either canned mock data or the real gortex daemon. Same model, same
// agent loop, the only variable is the backend.
//
//	go build -tags llama -o /tmp/agentdemo ./internal/llm/cmd/agentdemo
//	/tmp/agentdemo -model ~/models/qwen2.5-3b-instruct-q4_k_m.gguf \
//	               -question 'who calls LoadConfig?'
//	/tmp/agentdemo -backend daemon -repo gortex \
//	               -model ~/models/qwen2.5-3b-instruct-q4_k_m.gguf \
//	               -question 'who calls handleSearchSymbols?'
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/agent"
)

const promptP1 = `RULES (follow these exactly):
- If the user gives you only a bare name (not a path-qualified id like "pkg/x.Foo"), you MUST first call search_symbols to resolve it to an id before calling get_callers.
- For search_symbols, pass ONLY the bare symbol name as "query" — no prepositions, no package qualifiers, no extra words. Do not write "Foo in pkg/bar" — write just "Foo".
- search_symbols returns ranked matches; the FIRST few are best. Do not walk every match — pick at most the top 1-3 that look like functions or methods and ignore the rest (fields, params, strings).
- Make at least one real tool call before final_answer.
- Never call the same tool with the same args twice in a row. If a tool result is empty, try a DIFFERENT tool or DIFFERENT args.
- When you have gathered enough information, call final_answer summarising what you found.`

const promptP2 = promptP1 + `

EXAMPLE — answer "who calls Foo?":
Step 1 emit: {"tool":"search_symbols","args":{"query":"Foo"}}
Step 1 result: {"matches":[{"id":"pkg/x.Foo","kind":"function","path":"pkg/x/foo.go"}]}
Step 2 emit: {"tool":"get_callers","args":{"id":"pkg/x.Foo"}}
Step 2 result: {"callers":[{"id":"pkg/y.bar","file":"pkg/y/bar.go"}]}
Step 3 emit: {"tool":"final_answer","args":{"text":"Foo is called by pkg/y.bar in pkg/y/bar.go."}}`

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
     Repeat until you reach a symbol in a third repo OR you have enough to answer.
  5) Look for deps whose repo prefix differs from the handler's repo —
     those are the cross-repo downstream calls.
  6) Call final_answer with the chain as numbered steps.
- Never call the same tool with the same args twice in a row.
- final_answer.text should list each system hop with its symbol id and repo.`

func promptByName(name string) (string, error) {
	switch name {
	case "", "p0":
		return "", nil
	case "p1":
		return promptP1, nil
	case "p2":
		return promptP2, nil
	case "chain":
		return promptChain, nil
	}
	return "", fmt.Errorf("unknown prompt variant %q (use p0|p1|p2|chain)", name)
}

func main() {
	modelPath := flag.String("model", "", "path to .gguf model (required)")
	question := flag.String("question", "who calls LoadConfig?", "question for the agent")
	maxSteps := flag.Int("steps", 16, "max agent steps before giving up")
	nCtx := flag.Int("ctx", 4096, "context size")
	gpu := flag.Int("gpu", 999, "GPU layers to offload (Metal); 0 = CPU only")
	tmplName := flag.String("template", "chatml", "chat template: chatml | llama3")
	backendName := flag.String("backend", "mock", "backend: mock | daemon")
	gortexBin := flag.String("gortex-bin", "", "path to gortex binary (default: gortex on PATH)")
	repo := flag.String("repo", "", "restrict queries to this repo prefix")
	project := flag.String("project", "", "restrict queries to this project")
	ref := flag.String("ref", "", "restrict queries to this ref tag")
	promptName := flag.String("prompt", "p2", "prompt variant: p0 | p1 | p2 | chain")
	chainMode := flag.Bool("chain", false, "register contract + dependency tools for cross-system tracing")
	showGrammar := flag.Bool("show-grammar", false, "print the generated GBNF and exit")
	flag.Parse()

	systemExtras, err := promptByName(*promptName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	tmpl, err := agent.TemplateByName(*tmplName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if *modelPath == "" {
		fmt.Fprintln(os.Stderr, "error: -model is required")
		os.Exit(2)
	}

	var backend llm.Backend
	switch *backendName {
	case "mock":
		backend = llm.MockBackend{}
	case "daemon":
		db, err := llm.NewDaemonBackend(*gortexBin, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon backend: %v\n", err)
			os.Exit(1)
		}
		defer db.Close()
		backend = db
		fmt.Fprintf(os.Stderr, "[daemon backend: gortex mcp subprocess]\n")
	default:
		fmt.Fprintf(os.Stderr, "unknown -backend %q (use mock | daemon)\n", *backendName)
		os.Exit(2)
	}
	scope := llm.Scope{Repo: *repo, Project: *project, Ref: *ref}
	var tools []agent.Tool
	if *chainMode {
		tools = agent.GortexChainTools(backend, scope)
	} else {
		tools = agent.GortexTools(backend, scope)
	}

	m, err := llm.LoadModel(*modelPath, *gpu)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}
	defer m.Close()

	ctx, err := m.NewContext(*nCtx, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "context: %v\n", err)
		os.Exit(1)
	}
	defer ctx.Close()

	ag, err := agent.New(ctx, tools, tmpl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent: %v\n", err)
		os.Exit(1)
	}

	if *showGrammar {
		fmt.Println(ag.Grammar())
		return
	}

	t0 := time.Now()
	answer, transcript, runErr := ag.Run(systemExtras, *question, *maxSteps)

	fmt.Println("=== TRANSCRIPT ===")
	for i, st := range transcript {
		switch st.Kind {
		case "call":
			fmt.Printf("[%d] CALL   %s\n", i, st.Raw)
		case "result":
			fmt.Printf("[%d] RESULT %s\n", i, st.Raw)
		case "final":
			fmt.Printf("[%d] FINAL  %s\n", i, st.Raw)
		}
	}
	fmt.Println()
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "run error: %v\n", runErr)
		os.Exit(1)
	}
	fmt.Println("=== ANSWER ===")
	fmt.Println(answer)
	fmt.Fprintf(os.Stderr, "\n[elapsed %s]\n", time.Since(t0).Round(time.Millisecond))
}

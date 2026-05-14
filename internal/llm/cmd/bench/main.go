//go:build llama

// bench: run a fixed battery of agent questions across multiple GGUF
// models and print a comparison table. Reuses the agent package and
// the same mock tools as agentdemo so the only variable is the model.
//
//	go build -tags llama -o /tmp/llmbench ./internal/llm/cmd/bench
//	/tmp/llmbench
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/agent"
	"github.com/zzet/gortex/internal/llm/provider"
)

type modelSpec struct {
	Label    string
	Path     string
	Template string
}

type question struct {
	Name  string
	Text  string
	Scope llm.Scope // per-question override; empty fields fall back to the run-level scope
}

// effectiveScope returns the per-question scope merged with the
// run-level scope. Question-level fields win when set.
func (q question) effectiveScope(run llm.Scope) llm.Scope {
	s := run
	if q.Scope.Repo != "" {
		s.Repo = q.Scope.Repo
	}
	if q.Scope.Project != "" {
		s.Project = q.Scope.Project
	}
	if q.Scope.Ref != "" {
		s.Ref = q.Scope.Ref
	}
	return s
}

type result struct {
	Question string
	Steps    int
	Answer   string
	Err      string
	Elapsed  time.Duration
}

// Prompt variants. P0 is the empty baseline (no extras). P1 adds the
// process rules we expect to fix the failures observed in the model
// bench. P2 adds a one-shot worked example on top of P1.
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

// defaultQuestions returns the canonical bench question set for the
// given backend. The mock backend uses synthetic LoadConfig/RunServer
// questions; the daemon backend uses real cross-repo questions whose
// ground truth lives in the actual graph.
func defaultQuestions(backend string, chain bool) []question {
	if chain {
		return []question{
			{
				Name: "C1-chain-v1-stats",
				Text: `A TypeScript client in the web repo calls GET /v1/stats. Trace the chain: find the consumer symbol in the web repo, the provider handler in another repo via the contracts tool, and any cross-repo function the handler reaches via its dependencies. List the full chain as numbered steps.`,
			},
		}
	}
	if backend == "daemon" {
		return []question{
			{
				Name:  "Q1-newserver",
				Text:  "Who calls NewServer in the mcp package?",
				Scope: llm.Scope{Repo: "gortex"},
			},
			{
				Name:  "Q2-dialto",
				Text:  "Who calls DialTo?",
				Scope: llm.Scope{Repo: "gortex"},
			},
			{
				Name:  "Q3-recordwebhook",
				Text:  "Who calls RecordWebhookEvent?",
				Scope: llm.Scope{Repo: "gortex-cloud"},
			},
			{
				Name:  "Q4-encodeany",
				Text:  "Who calls EncodeAny?",
				Scope: llm.Scope{Repo: "gcx-go"},
			},
		}
	}
	return []question{
		{Name: "Q1-loadcfg", Text: "who calls the function called LoadConfig?"},
		{Name: "Q2-runserver", Text: "Find every caller of RunServer and tell me which file each lives in."},
		{Name: "Q3-nonexistent", Text: "Who calls FooBarBazNonexistent?"},
	}
}

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

func runOne(spec modelSpec, qs []question, ctxSize int, systemExtras string, backend llm.Backend, runScope llm.Scope, chain bool) []result {
	results := make([]result, 0, len(qs))

	cfg := llm.Config{
		Provider: "local",
		Local: llm.LocalConfig{
			Model:     spec.Path,
			Ctx:       ctxSize,
			GPULayers: 999,
			Template:  spec.Template,
		},
	}.ApplyDefaults()
	prov, err := provider.New(cfg)
	if err != nil {
		for _, q := range qs {
			results = append(results, result{Question: q.Name, Err: "provider: " + err.Error()})
		}
		return results
	}
	defer prov.Close()

	for _, q := range qs {
		// Rebuild tools per question so the scope can differ across
		// the cross-repo set. Chain mode adds contracts +
		// get_dependencies for cross-system tracing.
		var tools []agent.Tool
		if chain {
			tools = agent.GortexChainTools(backend, q.effectiveScope(runScope))
		} else {
			tools = agent.GortexTools(backend, q.effectiveScope(runScope))
		}
		ag, err := agent.New(prov, tools)
		if err != nil {
			results = append(results, result{Question: q.Name, Err: "agent: " + err.Error()})
			continue
		}

		maxSteps := 16
		if chain {
			maxSteps = 20
		}
		t0 := time.Now()
		ans, transcript, runErr := ag.Run(context.Background(), systemExtras, q.Text, maxSteps)
		elapsed := time.Since(t0)

		r := result{Question: q.Name, Steps: stepCount(transcript), Answer: ans, Elapsed: elapsed}
		if runErr != nil {
			r.Err = runErr.Error()
		}
		results = append(results, r)
	}
	return results
}

func stepCount(steps []agent.Step) int {
	n := 0
	for _, s := range steps {
		if s.Kind == "call" || s.Kind == "final" {
			n++
		}
	}
	return n
}

func main() {
	modelsDir := flag.String("models-dir", os.ExpandEnv("$HOME/models"), "directory containing .gguf files")
	ctxSize := flag.Int("ctx", 4096, "context size")
	promptName := flag.String("prompt", "p0", "prompt variant: p0 | p1 | p2")
	only := flag.String("only", "", "substring filter on model label; empty = all")
	backendName := flag.String("backend", "mock", "backend: mock | daemon")
	gortexBin := flag.String("gortex-bin", "", "path to gortex binary (default: gortex on PATH)")
	repo := flag.String("repo", "", "restrict queries to this repo prefix (cross-repo experiment)")
	project := flag.String("project", "", "restrict queries to this project")
	ref := flag.String("ref", "", "restrict queries to this ref tag")
	chain := flag.Bool("chain", false, "register contract+dependency tools, swap to cross-system chain question set")
	flag.Parse()
	if *chain && *promptName == "p0" {
		*promptName = "chain"
	}

	systemExtras, err := promptByName(*promptName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
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
	fmt.Fprintf(os.Stderr, "[prompt=%s backend=%s scope=%+v]\n", *promptName, *backendName, scope)

	specs := []modelSpec{
		{Label: "Qwen2.5-1.5B-Instruct (baseline)",
			Path:     filepath.Join(*modelsDir, "qwen2.5-1.5b-instruct-q4_k_m.gguf"),
			Template: "chatml"},
		{Label: "Qwen2.5-3B-Instruct (baseline)",
			Path:     filepath.Join(*modelsDir, "qwen2.5-3b-instruct-q4_k_m.gguf"),
			Template: "chatml"},
		{Label: "Qwen2.5-Coder-3B-Instruct",
			Path:     filepath.Join(*modelsDir, "qwen2.5-coder-3b-instruct-q4_k_m.gguf"),
			Template: "chatml"},
		{Label: "Hammer2.1-1.5b (tool-use specialist)",
			Path:     filepath.Join(*modelsDir, "Hammer2.1-1.5b.Q4_K_M.gguf"),
			Template: "chatml"},
		{Label: "Hermes-3-Llama-3.2-3B",
			Path:     filepath.Join(*modelsDir, "Hermes-3-Llama-3.2-3B.Q4_K_M.gguf"),
			Template: "chatml"},
		{Label: "Qwen2.5-Coder-7B-Instruct",
			Path:     filepath.Join(*modelsDir, "qwen2.5-coder-7b-instruct-q4_k_m.gguf"),
			Template: "chatml"},
	}

	questions := defaultQuestions(*backendName, *chain)

	all := make(map[string][]result)
	for _, s := range specs {
		if *only != "" && !strings.Contains(s.Label, *only) {
			continue
		}
		if _, err := os.Stat(s.Path); err != nil {
			fmt.Fprintf(os.Stderr, "[skip %s: %v]\n", s.Label, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "\n=== %s ===\n", s.Label)
		rs := runOne(s, questions, *ctxSize, systemExtras, backend, scope, *chain)
		all[s.Label] = rs
		for _, r := range rs {
			fmt.Fprintf(os.Stderr, "  %s  steps=%d  %s\n", r.Question, r.Steps, r.Elapsed.Round(time.Millisecond))
			if r.Err != "" {
				fmt.Fprintf(os.Stderr, "    ERROR: %s\n", r.Err)
			} else {
				fmt.Fprintf(os.Stderr, "    ANSWER: %s\n", trunc(r.Answer, 200))
			}
		}
	}

	// Final summary table on stdout — column-count adapts to the
	// question set so 3-question (mock) and 4-question (daemon) sets
	// both render correctly.
	fmt.Println()
	header := []string{"MODEL"}
	sep := []string{"------"}
	for _, q := range questions {
		header = append(header, q.Name+" steps/time")
		sep = append(sep, "---------------")
	}
	fmt.Println(strings.Join(header, " | "))
	fmt.Println(strings.Join(sep, "-|-"))
	for _, s := range specs {
		if *only != "" && !strings.Contains(s.Label, *only) {
			continue
		}
		rs, ok := all[s.Label]
		if !ok {
			continue
		}
		cells := []string{s.Label}
		for _, r := range rs {
			if r.Err != "" {
				cells = append(cells, "ERR")
			} else {
				cells = append(cells, fmt.Sprintf("%d / %s", r.Steps, r.Elapsed.Round(time.Millisecond)))
			}
		}
		fmt.Println(strings.Join(cells, " | "))
	}
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

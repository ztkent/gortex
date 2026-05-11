//go:build llama

// llmtest: minimal end-to-end smoke test of the internal/llm CGO
// wrapper. Loads a GGUF, runs greedy decoding from a prompt, streams
// the output to stdout.
//
//	go build -tags llama -o /tmp/llmtest ./internal/llm/cmd/llmtest
//	/tmp/llmtest -model ~/models/qwen2.5-0.5b-instruct-q4_k_m.gguf \
//	             -prompt 'List three Go web frameworks.'
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	llm "github.com/zzet/gortex/internal/llm"
)

// qwenChat wraps a user message in Qwen2.5's chat template. Hardcoded
// here so we don't need llama_chat_apply_template bindings yet.
func qwenChat(system, user string) string {
	if system == "" {
		system = "You are a helpful assistant."
	}
	return "<|im_start|>system\n" + system + "<|im_end|>\n" +
		"<|im_start|>user\n" + user + "<|im_end|>\n" +
		"<|im_start|>assistant\n"
}

func main() {
	modelPath := flag.String("model", "", "path to .gguf model (required)")
	prompt := flag.String("prompt", "Say hello in one short sentence.", "user prompt")
	system := flag.String("system", "", "system prompt (optional)")
	maxTok := flag.Int("max", 256, "max tokens to generate")
	nCtx := flag.Int("ctx", 2048, "context size")
	gpu := flag.Int("gpu", 999, "number of layers to offload to GPU (Metal); 0 = CPU only")
	threads := flag.Int("threads", 0, "CPU threads (0 = llama.cpp default)")
	raw := flag.Bool("raw", false, "use -prompt as raw text (no Qwen chat template)")
	flag.Parse()

	if *modelPath == "" {
		fmt.Fprintln(os.Stderr, "error: -model is required")
		flag.Usage()
		os.Exit(2)
	}

	tLoad := time.Now()
	m, err := llm.LoadModel(*modelPath, *gpu)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}
	defer m.Close()
	fmt.Fprintf(os.Stderr, "[loaded model in %s]\n", time.Since(tLoad).Round(time.Millisecond))

	ctx, err := m.NewContext(*nCtx, *threads)
	if err != nil {
		fmt.Fprintf(os.Stderr, "context: %v\n", err)
		os.Exit(1)
	}
	defer ctx.Close()

	text := *prompt
	if !*raw {
		text = qwenChat(*system, *prompt)
	}

	tGen := time.Now()
	n, err := ctx.Generate(text, *maxTok, func(piece string) bool {
		fmt.Print(piece)
		return true
	})
	fmt.Println()
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate: %v\n", err)
		os.Exit(1)
	}
	dt := time.Since(tGen)
	tps := float64(n) / dt.Seconds()
	fmt.Fprintf(os.Stderr, "[%d tokens in %s, %.1f tok/s]\n",
		n, dt.Round(time.Millisecond), tps)
}

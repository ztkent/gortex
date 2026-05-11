//go:build llama

// daemonprobe: smoke-test that DaemonBackend can drive `gortex mcp`
// against the live daemon and parse its responses. No LLM involved —
// just the backend wire.
//
//	go build -tags llama -o /tmp/daemonprobe ./internal/llm/cmd/daemonprobe
//	/tmp/daemonprobe -gortex-bin /path/to/gortex
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/zzet/gortex/internal/llm"
)

func main() {
	gortexBin := flag.String("gortex-bin", "", "path to gortex binary")
	query := flag.String("query", "handleSearchSymbols", "search_symbols query")
	repo := flag.String("repo", "", "scope: repo")
	flag.Parse()

	db, err := llm.NewDaemonBackend(*gortexBin, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx := context.Background()
	scope := llm.Scope{Repo: *repo}

	fmt.Println("--- list_repos ---")
	repos, err := db.ListRepos(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list_repos: %v\n", err)
	}
	b, _ := json.MarshalIndent(repos, "", "  ")
	fmt.Println(string(b))

	fmt.Println()
	fmt.Printf("--- search_symbols query=%q scope=%+v ---\n", *query, scope)
	matches, err := db.SearchSymbols(ctx, *query, scope, 5)
	if err != nil {
		fmt.Fprintf(os.Stderr, "search_symbols: %v\n", err)
		return
	}
	b, _ = json.MarshalIndent(matches, "", "  ")
	fmt.Println(string(b))

	if len(matches) == 0 {
		fmt.Println("(no matches; can't test get_callers)")
		return
	}
	target := matches[0].ID
	fmt.Println()
	fmt.Printf("--- get_callers id=%q ---\n", target)
	callers, err := db.GetCallers(ctx, target, scope, 20)
	if err != nil {
		fmt.Fprintf(os.Stderr, "get_callers: %v\n", err)
		return
	}
	b, _ = json.MarshalIndent(callers, "", "  ")
	fmt.Println(string(b))
}

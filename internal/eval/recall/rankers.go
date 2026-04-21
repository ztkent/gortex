package recall

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/search"
)

// BM25Ranker adapts a plain search.Backend to the Ranker shape. Works
// for either a raw BM25/Bleve backend or a HybridBackend's text side
// extracted via HybridBackend.TextBackend().
//
// Note: Gortex's indexer tokenizes symbol names at ingest time
// (Tokenize — camelCase-aware), but the query side (TokenizeQuery) does
// NOT split camelCase — so `backend.Search("NewServer", ...)` matches
// zero documents because the inverted index has `new` and `server`
// separately. The user-facing MCP search_symbols path avoids this by
// running through Engine.SearchSymbols, which adds a substring
// fallback. Use EngineRanker (below) to measure that full call path;
// use BM25Ranker only when you want the raw backend's behaviour.
func BM25Ranker(name string, backend search.Backend) Ranker {
	return Ranker{
		Name: name,
		Search: func(query string, limit int) []string {
			hits := backend.Search(query, limit)
			out := make([]string, len(hits))
			for i, h := range hits {
				out[i] = h.ID
			}
			return out
		},
	}
}

// EngineRanker measures what a real MCP caller sees via
// Engine.SearchSymbols — BM25/Bleve results + camelCase-friendly
// substring fallback. This is the recommended default for "bm25"-
// style evaluation; it reflects production behaviour.
func EngineRanker(name string, searchFn func(query string, limit int) []string) Ranker {
	return Ranker{Name: name, Search: searchFn}
}

// SemanticRanker adapts a vector backend + embedder to the Ranker
// shape. Returns ranker-level skip (empty slice + registered skip
// reason) when the vector backend is empty — callers can still emit
// a stable row for the report.
func SemanticRanker(name string, vector *search.VectorBackend, embedder embedding.Provider) Ranker {
	return Ranker{
		Name: name,
		Search: func(query string, limit int) []string {
			if vector == nil || embedder == nil || vector.Count() == 0 {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			vec, err := embedder.Embed(ctx, query)
			if err != nil || vec == nil {
				return nil
			}
			return vector.Search(vec, limit)
		},
	}
}

// RRFRanker adapts a HybridBackend (BM25 + vector fused via RRF) to the
// Ranker shape. Uses Search() which runs both sides and fuses when the
// vector backend has data — otherwise it degrades to BM25 gracefully.
func RRFRanker(name string, hybrid *search.HybridBackend) Ranker {
	return Ranker{
		Name: name,
		Search: func(query string, limit int) []string {
			hits := hybrid.Search(query, limit)
			out := make([]string, len(hits))
			for i, h := range hits {
				out[i] = h.ID
			}
			return out
		},
	}
}

// WinnowProvider is the minimal surface the WinnowRanker needs from the
// caller — an opaque hook that wraps the MCP server's WinnowForEval so
// we don't import internal/mcp from this retrieval-only package.
type WinnowProvider func(query string, extras map[string]any, limit int) []string

// WinnowRanker adapts a winnow provider to the Ranker shape. The
// provider runs the MCP server's graph-aware constraint scorer; per-
// case constraints flow through via Case.WinnowConstraints.
func WinnowRanker(name string, provide WinnowProvider, caseExtras func(query string) map[string]any) Ranker {
	return Ranker{
		Name: name,
		Search: func(query string, limit int) []string {
			var extras map[string]any
			if caseExtras != nil {
				extras = caseExtras(query)
			}
			return provide(query, extras, limit)
		},
	}
}

// RipgrepRanker shells out to `rg --files-with-matches` for each query
// and returns file paths as a ranked list. For any-hit recall we treat
// a hit as correct at rank K when any of the top-K file paths matches
// the file component of any Expected symbol ID.
//
// Gracefully skips if rg isn't on PATH — the Ranker will return an
// empty slice and the report surfaces it via the skip channel.
func RipgrepRanker(name, root string) Ranker {
	if _, err := exec.LookPath("rg"); err != nil {
		return Ranker{
			Name: name,
			Search: func(_ string, _ int) []string {
				return nil
			},
		}
	}
	// Map rg file paths into the graph's symbol-ID path prefix so
	// any-hit comparison works: an ID like "internal/foo.go::Bar"
	// matches a file hit like "internal/foo.go".
	return Ranker{
		Name: name,
		Search: func(query string, limit int) []string {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "rg",
				"--files-with-matches",
				"--no-messages",
				"--max-count", "1",
				"--", query, root)
			var outBuf bytes.Buffer
			cmd.Stdout = &outBuf
			cmd.Stderr = nil
			_ = cmd.Run() // non-zero exit means "no matches" — treat as empty
			files := make([]string, 0, 32)
			sc := bufio.NewScanner(&outBuf)
			for sc.Scan() {
				p := sc.Text()
				if p == "" {
					continue
				}
				rel, err := filepath.Rel(root, p)
				if err == nil {
					p = rel
				}
				files = append(files, filepath.ToSlash(p))
				if len(files) >= limit {
					break
				}
			}
			return files
		},
	}
}

// RipgrepAnyHit adapts a Ranker whose results are file paths (as
// produced by RipgrepRanker) to the any-hit recall model by checking
// whether the file prefix of any expected symbol ID matches any of
// the top-K ranked files. Returns a new Ranker whose Search still
// returns the raw file list — matching is done by rewriting the
// fixture Expected set before Run. Keep this as a helper so Run's
// code path stays uniform.
//
// Callers typically invoke AdaptCasesForFileRanker on their fixture
// before running the rg ranker.
func AdaptCasesForFileRanker(cases []Case) []Case {
	out := make([]Case, len(cases))
	for i, c := range cases {
		newExpected := make([]string, 0, len(c.Expected))
		seen := map[string]bool{}
		for _, id := range c.Expected {
			// Symbol IDs have the form "<file-path>::<symbol>". Split
			// on "::" and keep the file path; if there's no "::" the
			// ID is already a file path (or something we can't adapt).
			f := id
			if idx := strings.Index(id, "::"); idx >= 0 {
				f = id[:idx]
			}
			if !seen[f] {
				newExpected = append(newExpected, f)
				seen[f] = true
			}
		}
		out[i] = c
		out[i].Expected = newExpected
	}
	// Sort each expected list deterministically so test diffs are stable.
	for i := range out {
		sort.Strings(out[i].Expected)
	}
	return out
}

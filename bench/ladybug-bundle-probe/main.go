//go:build ladybug

// ladybug-bundle-probe: validates candidate Cypher patterns for the
// SymbolBundleSearcher capability — one engine call that returns the
// FTS hit + its full Node row + its in/out edges, so the rerank pipeline
// doesn't have to make 2-3 follow-up cgo round-trips per BM25 fan-out.
//
// Runs against an existing on-disk DB (default /tmp/gortex-daemon-lbug/store.lbug)
// already populated by the daemon. Tries the two candidate strategies:
//   A) one combined-MATCH+collect query (FTS YIELD + 2× OPTIONAL MATCH + collect)
//   B) two-query fallback (FTS → IDs, then batched bundle by IDs)
// then reports per-call wall-clock so we can pick the winner.
//
//	go run -tags ladybug ./bench/ladybug-bundle-probe -db /tmp/gortex-daemon-lbug/store.lbug \
//	  -queries "NewServer,handleStreamable,daemon controller"
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	lbug "github.com/LadybugDB/go-ladybug"

	"github.com/zzet/gortex/internal/search"
)

const ftsIndexName = "idx_symbol_fts_tokens"

func main() {
	dbPath := flag.String("db", "/tmp/gortex-daemon-lbug/store.lbug", "ladybug DB path")
	queriesArg := flag.String("queries", "NewServer,handleStreamable,daemon controller", "comma-separated FTS queries")
	iters := flag.Int("iters", 10, "iterations per measurement")
	limit := flag.Int("limit", 30, "FTS top-k")
	flag.Parse()

	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "db not found: %v\n", err)
		os.Exit(2)
	}
	db, err := lbug.OpenDatabase(*dbPath, lbug.DefaultSystemConfig())
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(2)
	}
	defer db.Close()
	conn, err := lbug.OpenConnection(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open conn: %v\n", err)
		os.Exit(2)
	}
	defer conn.Close()
	loadExtensions(conn)

	queries := strings.Split(*queriesArg, ",")
	for i, q := range queries {
		queries[i] = strings.TrimSpace(q)
	}

	// =====================================================================
	// Strategy A: single Cypher — FTS YIELD + OPTIONAL MATCH out + collect +
	//             OPTIONAL MATCH in + collect, returning the full bundle.
	// =====================================================================
	const cypherA = `
CALL QUERY_FTS_INDEX('SymbolFTS', '` + ftsIndexName + `', $q) RETURN node.id AS id, score
ORDER BY score DESC LIMIT $k`

	// Variant A1: FTS + per-row OPTIONAL MATCH collect (most ambitious).
	const cypherA1 = `
CALL QUERY_FTS_INDEX('SymbolFTS', '` + ftsIndexName + `', $q) RETURN node.id AS id, score
ORDER BY score DESC LIMIT $k`

	// Variant A2 (the actual bundle): FTS hits → IDs, then ONE batched
	// query that returns node + outEdges + inEdges via collect().
	const cypherA2OutFirst = `
MATCH (n:Node) WHERE n.id IN $ids
OPTIONAL MATCH (n)-[oe:Edge]->(to:Node)
WITH n, collect({to: to.id, kind: oe.kind, file_path: oe.file_path, line: oe.line, confidence: oe.confidence, confidence_label: oe.confidence_label, origin: oe.origin, tier: oe.tier, cross_repo: oe.cross_repo, meta: oe.meta}) AS outEdges
OPTIONAL MATCH (fr:Node)-[ie:Edge]->(n)
RETURN n.id, n.kind, n.name, n.qual_name, n.file_path, n.start_line, n.end_line, n.language, n.repo_prefix, n.workspace_id, n.project_id, n.meta,
       outEdges,
       collect({from: fr.id, kind: ie.kind, file_path: ie.file_path, line: ie.line, confidence: ie.confidence, confidence_label: ie.confidence_label, origin: ie.origin, tier: ie.tier, cross_repo: ie.cross_repo, meta: ie.meta}) AS inEdges`

	// =====================================================================
	// Strategy B: fallback — two queries.
	//   B1) FTS yields (id, score)
	//   B2a) one node-fetch (by ids) returning node columns + collected
	//        outEdges; B2b) one in-edge fetch by same ids.
	//   Cost: 1 FTS + 2 batched fetches, vs 1 FTS + 2 batched (today) — but
	//   the BIG win is that one BM25 call (the engine fires up to 2 today)
	//   now folds prepare()'s out+in edges into the same response — so the
	//   rerank can skip its own batched edge fetch when this is seeded.
	// =====================================================================
	const cypherBFTS = `
CALL QUERY_FTS_INDEX('SymbolFTS', '` + ftsIndexName + `', $q) RETURN node.id AS id, score
ORDER BY score DESC LIMIT $k`
	const cypherBOut = `
MATCH (a:Node)-[e:Edge]->(b:Node) WHERE a.id IN $ids
RETURN a.id, b.id, e.kind, e.file_path, e.line, e.confidence, e.confidence_label, e.origin, e.tier, e.cross_repo, e.meta`
	const cypherBIn = `
MATCH (a:Node)-[e:Edge]->(b:Node) WHERE b.id IN $ids
RETURN a.id, b.id, e.kind, e.file_path, e.line, e.confidence, e.confidence_label, e.origin, e.tier, e.cross_repo, e.meta`
	const cypherBNodes = `
MATCH (n:Node) WHERE n.id IN $ids
RETURN n.id, n.kind, n.name, n.qual_name, n.file_path, n.start_line, n.end_line, n.language, n.repo_prefix, n.workspace_id, n.project_id, n.meta`

	for _, qRaw := range queries {
		if qRaw == "" {
			continue
		}
		// Mirror the SymbolSearcher.SearchSymbols tokenisation: same
		// splitter the indexer uses on the write side.
		toks := search.Tokenize(qRaw)
		if len(toks) == 0 {
			toks = search.TokenizeQuery(qRaw)
		}
		q := strings.Join(toks, " ")
		fmt.Printf("\n========== query=%q (tokens=%q limit=%d) ==========\n", qRaw, q, *limit)

		// First, get the ids — needed for both A2 and B.
		idsRows, err := tryRun(conn, cypherA, map[string]any{"q": q, "k": int64(*limit)})
		if err != nil {
			fmt.Printf("  FTS A error: %v\n", err)
			continue
		}
		fmt.Printf("  FTS yielded %d ids\n", len(idsRows))
		ids := make([]any, 0, len(idsRows))
		for _, r := range idsRows {
			if id, ok := r[0].(string); ok {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			fmt.Printf("  no ids — skipping\n")
			continue
		}

		// --- Strategy A2: single combined OPTIONAL MATCH + collect ---
		fmt.Println("\n  -- Strategy A2: ONE bundle query (node + outEdges + inEdges via collect) --")
		var a2Rows int
		var a2OutCount, a2InCount int
		ok := medianAndMin(*iters, func() time.Duration {
			t := time.Now()
			rows, err := tryRun(conn, cypherA2OutFirst, map[string]any{"ids": ids})
			if err != nil {
				panic(err)
			}
			a2Rows = len(rows)
			// Inspect first row to verify shape
			if len(rows) > 0 && a2OutCount == 0 {
				row := rows[0]
				if len(row) >= 14 {
					if outE, ok := row[12].([]any); ok {
						a2OutCount = len(outE)
					}
					if inE, ok := row[13].([]any); ok {
						a2InCount = len(inE)
					}
				}
			}
			return time.Since(t)
		}, "A2 combined bundle")
		if ok {
			fmt.Printf("    rows=%d  sample out=%d in=%d edges/node\n", a2Rows, a2OutCount, a2InCount)
		}

		// --- Strategy B: separate fts + nodes + edges queries ---
		fmt.Println("\n  -- Strategy B: FTS + (nodes, outEdges, inEdges) split — 3 cgo trips after FTS --")
		medianAndMin(*iters, func() time.Duration {
			t := time.Now()
			rows, err := tryRun(conn, cypherBFTS, map[string]any{"q": q, "k": int64(*limit)})
			if err != nil {
				panic(err)
			}
			gotIDs := make([]any, 0, len(rows))
			for _, r := range rows {
				if id, ok := r[0].(string); ok {
					gotIDs = append(gotIDs, id)
				}
			}
			if len(gotIDs) == 0 {
				return time.Since(t)
			}
			args := map[string]any{"ids": gotIDs}
			if _, err := tryRun(conn, cypherBNodes, args); err != nil {
				panic(err)
			}
			if _, err := tryRun(conn, cypherBOut, args); err != nil {
				panic(err)
			}
			if _, err := tryRun(conn, cypherBIn, args); err != nil {
				panic(err)
			}
			return time.Since(t)
		}, "B FTS+nodes+out+in")

		// --- Sub-step B': just FTS (so we can subtract) ---
		medianAndMin(*iters, func() time.Duration {
			t := time.Now()
			if _, err := tryRun(conn, cypherBFTS, map[string]any{"q": q, "k": int64(*limit)}); err != nil {
				panic(err)
			}
			return time.Since(t)
		}, "    sub: FTS alone")

		// --- Sub-step B'': just nodes-by-ids (so we can subtract) ---
		medianAndMin(*iters, func() time.Duration {
			t := time.Now()
			if _, err := tryRun(conn, cypherBNodes, map[string]any{"ids": ids}); err != nil {
				panic(err)
			}
			return time.Since(t)
		}, "    sub: nodes by ids")

		// --- Sub-step B''': just out edges by ids (so we can subtract) ---
		medianAndMin(*iters, func() time.Duration {
			t := time.Now()
			if _, err := tryRun(conn, cypherBOut, map[string]any{"ids": ids}); err != nil {
				panic(err)
			}
			return time.Since(t)
		}, "    sub: outEdges by ids")

		medianAndMin(*iters, func() time.Duration {
			t := time.Now()
			if _, err := tryRun(conn, cypherBIn, map[string]any{"ids": ids}); err != nil {
				panic(err)
			}
			return time.Since(t)
		}, "    sub: inEdges by ids")
	}
}

func loadExtensions(conn *lbug.Connection) {
	for _, ext := range []string{"FTS", "ALGO", "VECTOR"} {
		res, err := conn.Query("LOAD EXTENSION " + ext)
		if err == nil && res != nil {
			res.Close()
		}
	}
}

func tryRun(conn *lbug.Connection, cypher string, args map[string]any) (rows [][]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
				return
			}
			err = fmt.Errorf("%v", r)
		}
	}()
	stmt, err := conn.Prepare(cypher)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	res, err := conn.Execute(stmt, args)
	if err != nil {
		return nil, err
	}
	defer res.Close()
	for res.HasNext() {
		tup, err := res.Next()
		if err != nil {
			return rows, err
		}
		vals, err := tup.GetAsSlice()
		if err != nil {
			tup.Close()
			return rows, err
		}
		rows = append(rows, vals)
		tup.Close()
	}
	return rows, nil
}

func medianAndMin(n int, fn func() time.Duration, label string) bool {
	if n <= 0 {
		n = 1
	}
	samples := make([]time.Duration, 0, n)
	var lastErr error
	for i := 0; i < n; i++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					lastErr = fmt.Errorf("%v", r)
				}
			}()
			samples = append(samples, fn())
		}()
		if lastErr != nil {
			fmt.Printf("    %s  ERROR: %v\n", label, lastErr)
			return false
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	min := samples[0]
	med := samples[len(samples)/2]
	max := samples[len(samples)-1]
	fmt.Printf("    %-50s  min=%-9s med=%-9s max=%s\n", label, min, med, max)
	return true
}

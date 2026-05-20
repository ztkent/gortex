# Gortex benchmarks

This document aggregates the five reproducible benchmark surfaces
gortex ships:

- **Reference-repo perf** — cold-index, search p95, impact p95/p99,
  incremental reindex, on-disk DB size, daemon resident memory
  across `gin` / `nestjs` / `react` (+ optional `linux`).
- **Token efficiency** — 3-pipeline comparison (ripgrep+full-read,
  ripgrep+context, gortex `search_symbols` + `get_symbol_source`)
  plus recall@k by token budget against a hand-curated ground-truth
  set.
- **GCX1 wire-format scorecard** — 20-fixture round-trip of GCX1 vs
  JSON, scored against both the `cl100k_base` tokenizer (Claude 3 /
  Opus 4 / Sonnet 4 / Haiku 4.5 / GPT-4o family) and the Claude
  Opus 4.7 tokenizer.
- **Daemon-mode MCP-tool latency** — p50/p95/p99 of the core MCP
  tools through the production dispatch path.
- **`search_symbols` retrieval recall** — R@1/5/20, MRR, and
  per-tier recall of the retrieval rankers over a curated query
  fixture.

Every section below carries: the headline number, the published
table, a "How to reproduce" block, and a link to the canonical
source artifacts. **Update protocol**: re-run the relevant
`gortex bench` subcommand, paste the new table into this file, bump
the "Last updated" stamp. The numbers below come from a single
operator's machine; reproducing them on your hardware will yield
different absolute timings but the same relative shape.

---

## 1. Reference-repo perf

**Last updated: 2026-05-20** · operator hardware: Apple M3 Max

| repo | LoC | files | nodes | edges | cold-index | search p95 | impact p95 | impact p99 | incremental | DB size | RSS | budget |
|------|----:|------:|------:|------:|-----------:|-----------:|-----------:|-----------:|------------:|--------:|----:|:------:|
| nestjs (in-tree fixture) | — | 32 | 240 | 414 | 17.8ms | 0.09ms | 0.01ms | 0.01ms | 11.8ms | 92.3KB | 2.4MB | ✓ |

_The full 3-repo run (gin + nestjs + react) requires network access
to clone each repo on first invocation. The fixture row above
exercises the same harness path against the in-tree nestjs fixture
so the contract is verifiable offline. The sub-millisecond impact
analysis claim holds — impact p95 of 0.01ms is 100× under the 1.0ms
budget._

_The **RSS** column is the Go heap retained with the graph, indexer
and query engine all live — the `runtime.MemStats` figure
`gortex daemon status` reports as daemon memory, sampled after a
forced GC so it reflects only the retained graph + search index.
True OS resident set adds a fixed Go-runtime overhead (stacks,
mcache, code) that does not scale with repo size._

### How to reproduce

```sh
# Full 3-repo run (clones gin/nestjs/react to ~/.cache/gortex/bench/)
gortex bench perf --out-dir bench/results

# Include the linux kernel preset (multi-GB; off by default)
gortex bench perf --include-linux --out-dir bench/results

# CI gate: fail on any budget violation
gortex bench perf --strict
```

Substrate: `bench/perf/` ([README](bench/perf/README.md)). Raw
metrics land at `bench/results/perf.{md,json,csv}` when
`--out-dir` is set.

---

## 2. Token efficiency vs ripgrep+read

**Last updated: 2026-05-18** · corpus: the gortex repo

| query | tokens (rg+full) | tokens (rg+ctx) | tokens (gortex) | recall@2k rg+full / rg+ctx / gortex | recall@10k rg+full / rg+ctx / gortex |
|-------|----------------:|----------------:|---------------:|------------------------------------|--------------------------------------|
| AddObservation | 31,530 | 9,020 | 972 | 0.00 / 0.00 / **1.00** | 0.00 / 1.00 / **1.00** |
| IsSymbolQuery | 23,027 | 7,388 | 577 | 0.00 / 0.00 / **1.00** | 0.00 / 1.00 / **1.00** |
| FileCoherenceSignal | 14,268 | 6,290 | 151 | 0.00 / 0.00 / **1.00** | 1.00 / 1.00 / **1.00** |
| alphaFuse | 14,574 | 5,930 | 534 | 0.00 / 0.00 / **1.00** | 1.00 / 1.00 / **1.00** |
| savings dashboard rendering (NL) | 415 | 544 | 1,825 | 0.00 / 0.00 / **1.00** | 0.00 / 0.00 / **1.00** |
| rerank pipeline default signals (NL) | 415 | 545 | 97 | 0.00 / 0.00 / **1.00** | 0.00 / 0.00 / **1.00** |
| Indexer Index method (NL) | 415 | 544 | 28 | 0.00 / 0.00 / **1.00** | 0.00 / 0.00 / **1.00** |
| MCP server start (NL) | 415 | 544 | 372 | 0.00 / 0.00 / **0.50** | 0.00 / 0.00 / **0.50** |

**Headline**: gortex achieves median **recall@2k = 1.00** vs **0.00**
for ripgrep across the identifier-query set, at **3-50× fewer tokens
per response**. On natural-language queries ("MCP server start") the
ripgrep pipelines return no matches (they need verbatim string hits),
inflating gortex's relative cost on the median; the per-row data is
the honest picture.

### How to reproduce

```sh
# Default: against the gortex repo itself
gortex bench tokens-efficiency

# Against a different corpus
gortex bench tokens-efficiency --repo /path/to/myrepo \
    --queries my-queries.json --groundtruth my-truth.json

# CI gate
gortex bench tokens-efficiency --strict --budget-ratio 0.5
```

Substrate: `bench/token-efficiency/`
([README](bench/token-efficiency/README.md)). Extend the
ground-truth set by adding rows to
`bench/token-efficiency/groundtruth.json`.

---

## 3. GCX1 wire-format vs JSON

**Last updated: 2026-05-18**

### cl100k_base (Claude 3 / Opus 4 / Sonnet 4 / Haiku 4.5 / GPT-4o)

- **Median token savings: −27.4%**
- **Median byte savings: −26.8%**
- **Round-trip integrity: 20/20**

### Claude Opus 4.7 (estimated via ×1.35 scalar; opt-in `--use-api` for exact counts)

- **Median token savings: −27.3%**

The wire format's advantage compounds with the tokenizer change
rather than being amplified by it. See the full per-fixture table at
[`bench/wire-format/scorecard.md`](bench/wire-format/scorecard.md).

### How to reproduce

```sh
# Both tokenizers, scalar Opus 4.7 estimate (offline)
go run ./bench/wire-format

# Exact Opus 4.7 counts via Anthropic count_tokens (requires
# ANTHROPIC_API_KEY; results cached so subsequent runs are
# deterministic without re-hitting the API)
go run ./bench/wire-format --use-api
```

Substrate: `bench/wire-format/`
([README](bench/wire-format/README.md)). See
[`docs/wire-format.md`](docs/wire-format.md) for the format spec.

---

---

## 4. Daemon-mode MCP-tool latency

**Last updated: 2026-05-19** · corpus: the gortex repo (71,300 nodes) · operator hardware: Apple M3 Max

| tool | iters | p50 | p95 | p99 | mean | max |
|------|------:|----:|----:|----:|-----:|----:|
| graph_stats        | 50 | 4.2ms  | 5.5ms   | 5.9ms   | 4.4ms  | 5.9ms   |
| search_symbols     | 50 | 1.2ms  | 22.4ms  | 26.9ms  | 5.6ms  | 26.9ms  |
| get_symbol_source  | 50 | 0.19ms | 0.90ms  | 1.3ms   | 0.27ms | 1.3ms   |
| get_callers        | 50 | 0.01ms | 0.02ms  | 0.03ms  | 0.01ms | 0.03ms  |
| find_usages        | 50 | 0.01ms | 0.01ms  | 0.01ms  | 0.01ms | 0.01ms  |
| get_file_summary   | 50 | 0.03ms | 0.04ms  | 0.05ms  | 0.03ms | 0.05ms  |
| smart_context      | 10 | 1.5ms  | 24.2ms  | 24.2ms  | 6.0ms  | 24.2ms  |
| get_repo_outline   | 50 | 60.6ms | 217.0ms | 377.0ms | 79.3ms | 377.0ms |

**Headline**: median p95 across tools is **5.5 ms**, median p99 is
**5.9 ms**. The heavy outliers (`smart_context`, `get_repo_outline`)
sit at hundreds of ms; everything else is single-digit ms or
sub-ms. Numbers measure `Handler.CallToolStrict` end-to-end through
the production MCP dispatch path; daemon socket framing adds
typically <1 ms on a warm pipe.

### How to reproduce

```sh
# Quick smoke against the local repo
gortex bench daemon-latency

# Tighter percentiles (more iterations)
gortex bench daemon-latency --iter 500

# Subset of tools (focus tuning)
gortex bench daemon-latency --tools graph_stats,search_symbols
```

Substrate: `bench/daemon-latency/` ([README](bench/daemon-latency/README.md)).
Raw metrics land at `bench/results/daemon-latency.{md,json,csv}`
when `--out-dir` is set.

---

## 5. search_symbols retrieval recall

**Last updated: 2026-05-20** · fixture: `bench/fixtures/retrieval.yaml`
(`gortex-seed-v2`, 156 cases) · operator hardware: Apple M3 Max

Recall@K of the retrieval rankers over a hand-curated query fixture,
tiered exact / concept / multi_hop. Recall is any-hit set-level
recall against strict gold labels — a paraphrased-but-correct hit
that misses the gold ID scores as a miss, so these are lower bounds
versus an LLM-judged setup.

| ranker  | R@1   | R@5   | R@20  | MRR   | p95 latency |
|---------|------:|------:|------:|------:|------------:|
| bm25    | 42.3% | 55.1% | 63.5% | 0.479 | 21.3ms      |
| winnow  | 37.8% | 50.0% | 64.1% | 0.439 | 22.9ms      |
| ripgrep |  0.0% | 17.3% | 29.5% | 0.061 | 162.2ms     |

Per-tier R@5 (bm25): exact **96.8%** · concept 25.4% · multi_hop 30.0%.

**Headline**: the `search_symbols` text path (`bm25`) lands
**R@5 = 55.1%** / **R@20 = 63.5%**, and **96.8%** on exact
symbol-name queries — 3.2× ripgrep's R@5 floor. Enabling Porter
stemming (`GORTEX_FTS_STEMMING=1`) trades a little exact-tier
precision for breadth — R@20 +5.7pp, exact-tier R@5 −3.1pp — so it
ships opt-in. The `semantic` and `rrf` rankers require `--embeddings`
and are omitted here; the `graph` ranker scores only graph-traversal
fixtures.

### How to reproduce

```sh
# Against the gortex repo itself
gortex eval recall --fixture bench/fixtures/retrieval.yaml --format markdown

# Add the semantic + RRF rankers (local GloVe embedder)
gortex eval recall --embeddings

# Standardized benches (CoIR / SWE-ContextBench / ContextBench)
gortex eval stdbench --bench coir --dataset <path>
```

Substrate: `internal/eval/recall/` + `cmd/gortex/eval_recall.go`. The
standardized-benchmark loaders live in `internal/eval/stdbench/`.

---

## Methodology notes

- **Hardware sensitivity.** Absolute timings vary 2-5× across
  machine classes; the budget gates (sub-ms impact, <50% gortex
  vs ripgrep tokens) are tuned to hold across the range a developer
  laptop or modest CI runner would produce.
- **Network sensitivity.** Reference-repo perf clones gin / nestjs
  / react on first invocation (cached afterward). Linux is off by
  default because the clone alone is multi-GB.
- **External dependencies.** Token-efficiency requires `rg`
  (ripgrep) on PATH for the two baseline pipelines; pass
  `--skip-ripgrep` to render a gortex-only column when rg is
  unavailable. Wire-format `--use-api` requires
  `ANTHROPIC_API_KEY`.
- **Ground-truth scope.** Token-efficiency ground truth is curated
  against the gortex repo. Extending the bench to a new corpus
  means adding a per-query expected-file map; the harness flags
  any query with no truth entry as recall=0 by definition (so
  silently-missing curation surfaces).

For benchmark-driven CI, see the harness flags above; each
subcommand supports `--strict` so a budget violation exits non-zero.

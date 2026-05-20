# Reference-repo perf benchmark

Reproducible perf table covering the cold-index → query → impact →
incremental round-trip across a fixed reference set: `gin`, `nestjs`,
`react`, and (opt-in) `linux`. Validates the sub-millisecond impact
analysis claim on real codebases and surfaces regressions in the
cold-index path before they ship.

## What it measures

For each repo the harness captures:

- **LoC / files / nodes / edges** — indexed surface size
- **Cold-index time** — `Indexer.Index(path)` wall time on a fresh
  graph
- **Search p95** — 50 representative queries (see `queries.json`)
  fanned through `Engine.SearchSymbols`; p50 + p95 reported
- **Impact p95 / p99** — 10 randomly-picked functions through
  `analysis.AnalyzeImpact`; the L4 sub-ms claim is enforced as a
  budget gate
- **Incremental re-index** — touch 5 files (`os.Chtimes`), re-run
  `Indexer.Index`, measure the delta
- **DB size** — estimated on-disk byte cost of the graph (gob-shaped,
  matches what a daemon snapshot would weigh)
- **RSS** — Go heap retained with the graph, indexer and query engine
  live (`runtime.MemStats` after a forced GC); the figure
  `gortex daemon status` reports as daemon memory

Output: stacked markdown (default) + companion CSV / JSON when
`-csv` / `-json` are passed. Per-row pass/fail markers; tail summary
with medians + violation count.

## Running

```sh
# Default 3-repo set (gin / nestjs / react), clones to ~/.cache/gortex/bench
go run ./bench/perf

# Include linux (multi-GB; off by default)
go run ./bench/perf -include-linux

# Local-only smoke run against an in-tree fixture
go run ./bench/perf -repos local:bench/fixtures/di/nestjs

# Strict mode for CI: exit 1 on any budget violation
go run ./bench/perf -strict -budget-impact-p95-ms 1.0
```

Flags:

- `-repos LIST` — comma-separated repo set; tokens accept preset
  slugs (`gin` / `nestjs` / `react` / `linux`), `owner/repo`
  shorthand, full HTTPS URLs, or `local:/path` for non-cloned repos
- `-include-linux` — opt in to the linux kernel preset
- `-cache-dir DIR` — clone cache (default `~/.cache/gortex/bench`)
- `-queries PATH` — JSON query set (default
  `bench/perf/queries.json`)
- `-out PATH` — primary output (default stdout)
- `-format` — `markdown` (default) / `csv` / `json`
- `-csv PATH` / `-json PATH` — companion outputs alongside the
  primary
- `-budget-impact-p95-ms` — impact p95 cap in ms (default `1.0`)
- `-budget-search-p95-ms` — search p95 cap in ms (default `50`)
- `-budget-cold-index-ms` — cold-index cap (default off — repos
  vary too widely)
- `-strict` — exit 1 when any budget gate is tripped

## CI usage

Calling the bench through `gortex bench perf` (wired in
`cmd/gortex/bench.go`) is the recommended path — picks up the same
flags via `--` plus the `--out-dir DIR` convention shared with
`bench tokens`.

The shipped reference numbers in `BENCHMARK.md` come from running
the default 3-repo set on an Apple M3 Max with a warm
`~/.cache/gortex/bench` directory.

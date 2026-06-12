# Token savings

Gortex tracks how many tokens it saves compared to naive file reads — per-call, per-session, and cumulative across restarts:

- **Per-call:** every source-reading tool — `read_file`, `get_file_summary`, `get_editing_context`, `get_symbol_source`, `batch_symbols` (with `include_source`), `smart_context` — books an observation server-side: tokens actually returned vs the full-file read the response stands in for. The per-call value is deliberately not echoed in responses (agents don't act on it and it would burn tokens on every reply); it lands in the ledger.
- **Session-level:** `graph_stats` returns a `token_savings` object with `calls_counted`, `tokens_returned`, `tokens_saved`, `efficiency_ratio`.
- **Cumulative (cross-session):** `graph_stats` also returns `cumulative_savings` when persistence is wired — includes `first_seen`, `last_updated`, and `cost_avoided_usd` per model (Claude Opus/Sonnet/Haiku, GPT-4o, GPT-4o-mini). Backed by the machine-global sidecar database (`~/.gortex/sidecar.sqlite` — the same file that holds notes/memories): `savings_totals` carries top-line + per-repo + per-language aggregates and `savings_events` one session-tagged row per call, powering the windowed buckets and the per-tool breakdown. Each observation commits transactionally, so the ledger survives SIGKILLed MCP servers and concurrent writer processes. Flat-file ledgers from older releases (`~/.gortex/cache/savings.json` + `savings.jsonl`) are imported once on first open and renamed `*.bak`.

`gortex savings` renders a three-bucket dashboard:

```text
Gortex Token Savings
====================
Cost avoided:   $168.69 (claude-opus-4) across 1,878 calls · 11,246,094 tokens saved

Today       ████████░░░░░░░░   50.0%  saved 9,200 / 18,400 tokens   $0.14
Last 7 days ██████████░░░░░░   62.5%  saved 60,100 / 96,200 tokens  $0.90
All time    ███████████████░   93.3%  saved 11,246,094 / 12,050,716 tokens  $168.69
```

```bash
# Three-bucket dashboard with USD on top
gortex savings

# Per-tool breakdown inside each bucket
gortex savings --verbose

# Headline a single model (fuzzy match: "opus" → claude-opus-4)
gortex savings --model opus

# Bucket "Today" by UTC instead of local time
gortex savings --utc

# Machine-readable output (mirrors the dashboard structure: buckets[].per_tool, cost_avoided_usd, etc.)
gortex savings --json

# Wipe cumulative totals and the event history
gortex savings --reset

# Override pricing (JSON array of {model, usd_per_m_input})
GORTEX_MODEL_PRICING_JSON='[{"model":"mycorp","usd_per_m_input":5}]' gortex savings
```

Token counts use **tiktoken (`cl100k_base`)** — the tokenizer Claude and GPT-4 actually use — via `github.com/pkoukk/tiktoken-go` with an embedded offline BPE loader, so no runtime downloads. The BPE is lazy-loaded on first call. If init fails for any reason, the package falls back to the legacy `chars/4` heuristic so metrics stay usable.

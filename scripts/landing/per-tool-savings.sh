#!/usr/bin/env bash
#
# Regenerate docs/landing-pages/per-tool-savings.md from the
# cumulative savings store + the JSONL event log. Three sections:
#
#   1. Headline totals (always populated — comes from the cumulative
#      store written every flush)
#   2. Per-language breakdown (always populated — same source)
#   3. Per-tool breakdown (populated once the JSONL event log has
#      accumulated calls; the JSONL surface was added 2026-05-18 so
#      stores predating that won't have per-tool data until enough
#      new sessions accumulate)
#
# Requires: gortex on PATH, jq.
# Usage:    bash scripts/landing/per-tool-savings.sh

set -euo pipefail

OUT="${OUT:-docs/landing-pages/per-tool-savings.md}"
SAVINGS_JSON="$(gortex savings --verbose --json)"

if ! echo "$SAVINGS_JSON" | jq -e '.calls_counted > 0' >/dev/null; then
    echo "per-tool-savings: cumulative savings store is empty" >&2
    echo "  (use gortex via MCP for a while, then re-run)" >&2
    exit 1
fi

mkdir -p "$(dirname "$OUT")"
generated_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

TOTAL_CALLS="$(echo "$SAVINGS_JSON" | jq -r '.calls_counted')"
TOTAL_SAVED="$(echo "$SAVINGS_JSON" | jq -r '.tokens_saved')"
TOTAL_RETURNED="$(echo "$SAVINGS_JSON" | jq -r '.tokens_returned')"
HEADLINE_COST_OPUS="$(echo "$SAVINGS_JSON" | jq -r '.cost_avoided_usd["claude-opus-4"]')"
HEADLINE_COST_SONNET="$(echo "$SAVINGS_JSON" | jq -r '.cost_avoided_usd["claude-sonnet-4"]')"
HEADLINE_COST_HAIKU="$(echo "$SAVINGS_JSON" | jq -r '.cost_avoided_usd["claude-haiku-4.5"]')"
HEADLINE_COST_GPT4O="$(echo "$SAVINGS_JSON" | jq -r '.cost_avoided_usd["gpt-4o"]')"
EFFICIENCY="$(echo "$SAVINGS_JSON" | jq -r 'if .tokens_returned > 0 then ((.tokens_saved + .tokens_returned) / .tokens_returned) else 0 end')"

cat > "$OUT" <<EOF
# Gortex token savings — published landing-page tables

**Last regenerated**: $generated_at  ·  Source: \`gortex savings
--verbose --json\` against the operator's cumulative store
(the \`~/.gortex/sidecar.sqlite\` savings ledger).

## Headline

| metric | value |
|--------|------:|
| Source-reading MCP calls | **$TOTAL_CALLS** |
| Tokens saved (vs naive file reads) | **$TOTAL_SAVED** |
| Tokens returned | $TOTAL_RETURNED |
| Efficiency ratio | $(printf '%.1fx' "$EFFICIENCY") |
| USD avoided · claude-opus-4 | **\$$(printf '%.2f' "$HEADLINE_COST_OPUS")** |
| USD avoided · claude-sonnet-4 | \$$(printf '%.2f' "$HEADLINE_COST_SONNET") |
| USD avoided · claude-haiku-4.5 | \$$(printf '%.2f' "$HEADLINE_COST_HAIKU") |
| USD avoided · gpt-4o | \$$(printf '%.2f' "$HEADLINE_COST_GPT4O") |

## By language

| language | calls | tokens saved | savings % |
|----------|------:|-------------:|----------:|
EOF

echo "$SAVINGS_JSON" | jq -r '
    .per_language | to_entries |
    sort_by(-.value.tokens_saved) |
    .[] |
    [.key, .value.calls_counted, .value.tokens_saved,
     (if (.value.tokens_saved + .value.tokens_returned) > 0
      then ((.value.tokens_saved / (.value.tokens_saved + .value.tokens_returned)) * 100 | floor)
      else 0 end)] |
    "| \(.[0]) | \(.[1]) | \(.[2]) | \(.[3])% |"
' >> "$OUT"

cat >> "$OUT" <<EOF

## By repository

| repo | calls | tokens saved |
|------|------:|-------------:|
EOF

echo "$SAVINGS_JSON" | jq -r '
    .per_repo | to_entries |
    sort_by(-.value.tokens_saved) |
    .[] |
    [.key, .value.calls_counted, .value.tokens_saved] |
    "| \(.[0]) | \(.[1]) | \(.[2]) |"
' >> "$OUT"

# Per-tool section: only populated when the JSONL event log has
# rows for the All-time bucket. Stores predating the JSONL surface
# (added 2026-05-18) will skip this section until enough new
# sessions accumulate. Honest about the gap rather than padding
# with synthetic numbers.
PER_TOOL_COUNT="$(echo "$SAVINGS_JSON" | jq -r '.buckets[2].per_tool | length // 0')"

cat >> "$OUT" <<EOF

## By MCP tool
EOF

if [[ "$PER_TOOL_COUNT" -gt 0 ]]; then
    cat >> "$OUT" <<EOF

| tool | calls | tokens saved | median saved/call |
|------|------:|-------------:|------------------:|
EOF
    echo "$SAVINGS_JSON" | jq -r '
        .buckets[2].per_tool |
        sort_by(-.tokens_saved) |
        .[] |
        [.tool, .calls_counted, .tokens_saved,
         (if .calls_counted > 0 then (.tokens_saved / .calls_counted | floor) else 0 end)] |
        "| \(.[0]) | \(.[1]) | \(.[2]) | \(.[3]) |"
    ' >> "$OUT"
else
    cat >> "$OUT" <<'EOF'

_Per-tool breakdown becomes available once the per-call JSONL
event log accumulates rows. The JSONL surface was added 2026-05-18;
cumulative stores predating that version won't show per-tool data
until enough new sessions populate the log. Re-run this script
after a week of usage to surface the table here._
EOF
fi

cat >> "$OUT" <<'EOF'

## Reproducing this page

```sh
bash scripts/landing/per-tool-savings.sh
```

The script reads from the cumulative savings store + JSONL event
log written by the MCP server every time a source-reading tool
returns. See `cmd/gortex/savings.go` for the dashboard the script
wraps, and `internal/savings/` for the persistence layer.

Override pricing for org-specific rates by exporting
`GORTEX_MODEL_PRICING_JSON` before running.
EOF

echo "wrote $OUT" >&2

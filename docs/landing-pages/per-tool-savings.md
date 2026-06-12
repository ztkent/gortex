# Gortex token savings — published landing-page tables

**Last regenerated**: 2026-05-18T22:20:29Z  ·  Source: `gortex savings
--verbose --json` against the operator's cumulative store
(the `~/.gortex/sidecar.sqlite` savings ledger).

## Headline

| metric | value |
|--------|------:|
| Source-reading MCP calls | **1926** |
| Tokens saved (vs naive file reads) | **11499977** |
| Tokens returned | 822377 |
| Efficiency ratio | 15.0x |
| USD avoided · claude-opus-4 | **$172.50** |
| USD avoided · claude-sonnet-4 | $34.50 |
| USD avoided · claude-haiku-4.5 | $11.50 |
| USD avoided · gpt-4o | $28.75 |

## By language

| language | calls | tokens saved | savings % |
|----------|------:|-------------:|----------:|
| go | 1700 | 10614468 | 93% |
| javascript | 17 | 150220 | 96% |
| markdown | 1 | 143989 | 99% |
| typescript | 41 | 41530 | 81% |
| rust | 5 | 37121 | 94% |
| python | 3 | 8920 | 82% |
| elixir | 3 | 8817 | 92% |
| bash | 1 | 3622 | 97% |
| dart | 2 | 393 | 69% |

## By repository

| repo | calls | tokens saved |
|------|------:|-------------:|
| gortex | 1010 | 6955565 |
| rate_checkers_detector | 101 | 602509 |
| . | 35 | 101507 |
| labrador | 13 | 57386 |
| daedalus | 16 | 24043 |
| bofh-api | 1 | 5290 |
| gortex-cloud | 2 | 4977 |
| bofh-rebooking | 1 | 3035 |
| gcx-go | 2 | 1624 |
| meta | 1 | 740 |
| tuck_app | 2 | 393 |
| ds-flows | 1 | 51 |

## By MCP tool

_Per-tool breakdown becomes available once the per-call JSONL
event log accumulates rows. The JSONL surface was added 2026-05-18;
cumulative stores predating that version won't show per-tool data
until enough new sessions populate the log. Re-run this script
after a week of usage to surface the table here._

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

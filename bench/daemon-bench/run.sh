#!/usr/bin/env bash
# Drive the daemon-bench binary against gortex daemon for each
# storage backend. Sequential — only one daemon up at a time so they
# can share the default unix socket.
#
# Inputs (env or arg defaults):
#   BIN              gortex binary to run                  (default: /tmp/gortex-lbug)
#   ADDR             http addr for the daemon               (default: 127.0.0.1:7090)
#   TOKEN            bearer token                           (default: x)
#   RESULTS_DIR      output dir for JSON + log per backend  (default: /tmp/daemon-bench-results)
#   BACKENDS         space-separated list of backend tags   (default: "memory ladybug")
#   LBUG_PATH        path for ladybug store dir             (default: /tmp/gortex-daemon-lbug/store.lbug)
#   WAIT_MAX_S       seconds to wait for warmup ready       (default: 240)

set -euo pipefail

BIN="${BIN:-/tmp/gortex-lbug}"
ADDR="${ADDR:-127.0.0.1:7090}"
TOKEN="${TOKEN:-x}"
RESULTS_DIR="${RESULTS_DIR:-/tmp/daemon-bench-results}"
BACKENDS="${BACKENDS:-memory ladybug}"
LBUG_PATH="${LBUG_PATH:-/tmp/gortex-daemon-lbug/store.lbug}"
WAIT_MAX_S="${WAIT_MAX_S:-240}"

mkdir -p "$RESULTS_DIR"

SOCK_PATH="$HOME/.cache/gortex/daemon.sock"

stop_daemon() {
    if [[ -n "${DAEMON_PID:-}" ]]; then
        if kill -0 "$DAEMON_PID" 2>/dev/null; then
            kill -TERM "$DAEMON_PID" 2>/dev/null || true
            for _ in {1..20}; do
                kill -0 "$DAEMON_PID" 2>/dev/null || break
                sleep 0.2
            done
            kill -KILL "$DAEMON_PID" 2>/dev/null || true
        fi
        DAEMON_PID=""
    fi
    rm -f "$SOCK_PATH"
    # give the OS a moment to release the TCP port
    sleep 0.3
}

trap 'stop_daemon' EXIT INT TERM

http_url() {
    # ADDR is host:port; strip a possible scheme if user added one.
    printf 'http://%s' "${ADDR#http://}"
}

wait_for_ready() {
    local log="$1"
    local started=$SECONDS
    while (( SECONDS - started < WAIT_MAX_S )); do
        if grep -q '"daemon: watching"' "$log" 2>/dev/null; then
            return 0
        fi
        if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
            echo "ERROR: daemon died during warmup. Last log:" >&2
            tail -40 "$log" >&2
            return 1
        fi
        sleep 0.5
    done
    echo "TIMEOUT after ${WAIT_MAX_S}s waiting for warmup. Tail:" >&2
    tail -40 "$log" >&2
    return 1
}

bench_one() {
    local backend="$1"
    local log="$RESULTS_DIR/daemon-$backend.log"
    local out="$RESULTS_DIR/results-$backend.json"
    local args=(--backend "$backend" --http-addr "$ADDR" --http-auth-token "$TOKEN")

    if [[ "$backend" == "ladybug" ]]; then
        # Fresh on-disk store every run so the cold-start path is honest.
        rm -rf "$(dirname "$LBUG_PATH")"
        mkdir -p "$(dirname "$LBUG_PATH")"
        args+=(--backend-path "$LBUG_PATH")
    fi

    # Ensure no stale daemon / socket from the previous backend.
    stop_daemon

    echo ""
    echo "==================================================================="
    echo "== Backend: $backend"
    echo "==================================================================="

    : >"$log"
    local start_epoch
    start_epoch=$(perl -e 'use Time::HiRes qw(time); printf "%.3f", time')

    # Launch the daemon detached: nohup ignores SIGHUP, redirect all
    # FDs so we don't inherit the parent shell's TTY. macOS lacks
    # `setsid`, so we use `disown` after the fork to detach from the
    # job table.
    nohup "$BIN" daemon start "${args[@]}" \
        >"$log" 2>&1 < /dev/null &
    DAEMON_PID=$!
    disown 2>/dev/null || true

    echo "[$backend] daemon launched (pid=$DAEMON_PID), log=$log"
    if ! wait_for_ready "$log"; then
        return 1
    fi

    local ready_epoch
    ready_epoch=$(perl -e 'use Time::HiRes qw(time); printf "%.3f", time')
    local warmup_s
    warmup_s=$(awk -v s="$start_epoch" -v r="$ready_epoch" 'BEGIN{printf "%.2f", r-s}')
    echo "[$backend] warmup → ready: ${warmup_s}s"

    # Wait a beat so any post-watcher_started bookkeeping settles.
    sleep 1

    echo "[$backend] running tool battery..."
    /tmp/daemon-bench \
        --addr "$(http_url)" \
        --token "$TOKEN" \
        --label "$backend" \
        --json "$out" \
    || echo "[$backend] daemon-bench exited non-zero (continuing)"

    echo "[$backend] saved $out"

    stop_daemon
    echo "[$backend] done."
}

# Build the bench binary once.
echo "== building daemon-bench =="
(cd "$(dirname "$0")/../.." && go build -o /tmp/daemon-bench ./bench/daemon-bench/)

# Run each backend in turn.
for backend in $BACKENDS; do
    bench_one "$backend" || echo "[$backend] FAILED, continuing"
done

echo ""
echo "==================================================================="
echo "== Summary"
echo "==================================================================="
for backend in $BACKENDS; do
    out="$RESULTS_DIR/results-$backend.json"
    if [[ -f "$out" ]]; then
        echo ""
        echo "-- $backend --"
        # Pretty-print headline numbers
        python3 - "$out" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    d = json.load(f)
print(f"label={d['label']}, total_ms={d['total_ms']}")
ok = sum(1 for r in d['records'] if r['ok'])
print(f"ok={ok}/{len(d['records'])}")
print(f"{'label':<44} {'ms':>8} {'bytes':>8}")
for r in d['records']:
    flag = '' if r['ok'] else '  ERR'
    print(f"{r['label']:<44} {r['elapsed_ms']:>8} {r['output_bytes']:>8}{flag}")
PY
    else
        echo "-- $backend -- (no result file)"
    fi
done

#!/usr/bin/env bash
# Sequential Linux-kernel bench for the disk backends
# (ladybug, duckdb, sqlite). Forces shadow swap via
# GORTEX_SHADOW_MAX_FILES so each backend gets the
# drain-shadow benefit.

set -euo pipefail

REPO_ROOT=/Volumes/ext_drive/code/oss/linux
SCRATCH_BASE=/Volumes/ext_drive/code/temp
RESULTS_DIR="$(cd "$(dirname "$0")/.." && pwd)/bench/results"
mkdir -p "$RESULTS_DIR" "$SCRATCH_BASE"

export GORTEX_SHADOW_MAX_FILES=200000
export TMPDIR="$SCRATCH_BASE"

run_backend() {
    local backend="$1"
    local binary="$2"
    local out="$RESULTS_DIR/linux-${backend}-drain"

    echo "================================================================"
    echo "[$(date +%H:%M:%S)] $backend"

    # wipe scratch *before* run
    rm -rf "$SCRATCH_BASE"/store-bench-* 2>/dev/null || true

    "$binary" -workers=8 -root="$REPO_ROOT" -only="$backend" \
        > "$out.md" 2> "$out.stderr" || echo "[$(date +%H:%M:%S)] $backend FAILED"

    echo "[$(date +%H:%M:%S)] $backend done — result:"
    cat "$out.md" | tail -3
    echo
    # wipe scratch *after* run too
    rm -rf "$SCRATCH_BASE"/store-bench-* 2>/dev/null || true
}

run_backend ladybug /tmp/bench-main
run_backend duckdb  /tmp/bench-main
run_backend sqlite  /tmp/bench-main

echo "================================================================"
echo "[$(date +%H:%M:%S)] all done."

#!/usr/bin/env bash
# Sequential Linux-kernel bench across all viable disk backends.
# Cleans the scratch dir between runs so disk usage stays bounded.
#
# Streaming flush is engaged automatically by GORTEX_STREAMING_FLUSH=1
# above the shadow-max threshold (default 50k files). Linux has ~64k
# source files, so streaming flush keeps RAM bounded by chunking the
# parse phase to per-chunk in-memory shadows that are flushed to disk
# between chunks.

set -euo pipefail

REPO_ROOT=/Volumes/ext_drive/code/oss/linux
SCRATCH_BASE=/Volumes/ext_drive/code/temp
RESULTS_DIR="$(cd "$(dirname "$0")/.." && pwd)/bench/results"
mkdir -p "$RESULTS_DIR" "$SCRATCH_BASE"

# Bound peak RAM: chunk parse at 4000 files (~480MB shadow each).
export GORTEX_STREAMING_FLUSH=1
export GORTEX_STREAMING_CHUNK_SIZE=4000

# Tell Go to put its own scratch dirs on the ext drive so the tiny
# system disk doesn't fill from Bleve / duckdb tempfiles.
export TMPDIR="$SCRATCH_BASE/gortex-tmp"
mkdir -p "$TMPDIR"

run_backend() {
    local backend="$1"
    local binary="$2"
    local scratch="$SCRATCH_BASE/bench-$backend"
    local out="$RESULTS_DIR/linux-${backend}-v1"

    echo "================================================================"
    echo "[$(date +%H:%M:%S)] $backend — wiping scratch $scratch"
    rm -rf "$scratch"
    mkdir -p "$scratch"

    # The bench's MkdirTemp uses TMPDIR; the scratch dir we just made
    # gets pointed at via TMPDIR for this single backend.
    TMPDIR="$scratch" "$binary" -workers=8 -root="$REPO_ROOT" -only="$backend" \
        > "$out.md" 2> "$out.stderr" || echo "[$(date +%H:%M:%S)] $backend FAILED"

    echo "[$(date +%H:%M:%S)] $backend done — result:"
    cat "$out.md" | tail -5
    echo
    # Clean up — both the bench's temp DB dir and any TMPDIR spill.
    rm -rf "$scratch"
}

run_backend ladybug /tmp/bench-main
run_backend duckdb  /tmp/bench-main
run_backend sqlite  /tmp/bench-main

echo "================================================================"
echo "[$(date +%H:%M:%S)] all backends done. Results in $RESULTS_DIR/linux-*"

#!/usr/bin/env python3
"""
Reproduction for issue #40 — "Claude Code eats up context when using gortex".

The report's core, *measurable* claim is:

  - During plan implementation, files get read in FULL via gortex's read_file /
    get_editing_context, which is token-expensive.
  - compress_bodies:true and/or search_text are far cheaper, but nothing forces
    (or even nudges toward) them — read_file defaults to compress_bodies:false.

This script confirms/disproves the *measurable* part by driving the REAL tools
through the running daemon (via `gortex mcp --proxy`) and comparing the wire
cost of three access patterns on the same set of files:

  1. read_file                      (full bodies — the "eats context" path)
  2. read_file compress_bodies:true (signatures + structure only)
  3. search_text                    (locate call sites, no body read at all)

Token figures are estimated at ~bytes/4 (the standard rough heuristic; Claude's
real tokenizer differs but the RATIO between patterns is what matters and is
tokenizer-stable). The script reports raw bytes too, so nothing hinges on the
estimate.

Usage:
  python3 bench/issue40_context_repro.py [GORTEX_BIN] [file ...]

Defaults: ./gortex and a handful of ~14-24KB Go files (≈ the reporter's C++
file sizes). Pass a repo-prefixed or absolute path per file (e.g.
gortex/internal/resolver/external_calls.go).
"""
import json
import subprocess
import sys
import threading

GORTEX_BIN = sys.argv[1] if len(sys.argv) > 1 else "./gortex"
FILES = sys.argv[2:] or [
    "gortex/internal/resolver/external_calls.go",
    "gortex/internal/mcp/tools_lsp.go",
    "gortex/internal/agents/claudecode/plugin.go",
    "gortex/internal/parser/languages/swift.go",
]
# A literal that recurs across the repo — the search_text "locate the call
# sites" pattern the reporter says should have been used instead of reads.
SEARCH_QUERY = "zap.Error"


def approx_tokens(nbytes: int) -> int:
    return round(nbytes / 4)


class MCP:
    """Minimal newline-delimited JSON-RPC client over `gortex mcp --proxy`."""

    def __init__(self, binary):
        self.p = subprocess.Popen(
            [binary, "mcp", "--proxy", "--log-level", "error"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, bufsize=1,
        )
        self._id = 0
        # Drain stderr so a chatty daemon can't dead-lock the pipe.
        self._err = []
        threading.Thread(target=self._drain_err, daemon=True).start()

    def _drain_err(self):
        for line in self.p.stderr:
            self._err.append(line)

    def _send(self, method, params=None, notify=False):
        msg = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            msg["params"] = params
        if not notify:
            self._id += 1
            msg["id"] = self._id
        self.p.stdin.write(json.dumps(msg) + "\n")
        self.p.stdin.flush()
        if notify:
            return None
        return self._read_result(self._id)

    def _read_result(self, want_id):
        while True:
            line = self.p.stdout.readline()
            if not line:
                raise RuntimeError(
                    "daemon closed the connection.\nstderr:\n" + "".join(self._err[-20:]))
            try:
                msg = json.loads(line)
            except json.JSONDecodeError:
                continue  # skip log noise that leaked onto stdout
            if msg.get("id") == want_id:
                if "error" in msg:
                    raise RuntimeError(f"RPC error: {msg['error']}")
                return msg.get("result")

    def initialize(self):
        self._send("initialize", {
            "protocolVersion": "2025-06-18",
            "capabilities": {},
            "clientInfo": {"name": "issue40-repro", "version": "0"},
        })
        self._send("notifications/initialized", notify=True)

    def call(self, name, args):
        res = self._send("tools/call", {"name": name, "arguments": args})
        # Concatenate all text content blocks — that is what lands in the model's
        # context window.
        parts = []
        for block in (res or {}).get("content", []):
            if block.get("type") == "text":
                parts.append(block.get("text", ""))
        return "".join(parts)

    def close(self):
        try:
            self.p.stdin.close()
        except Exception:
            pass
        self.p.terminate()


def main():
    mcp = MCP(GORTEX_BIN)
    try:
        mcp.initialize()
        print(f"Driving real tools via `{GORTEX_BIN} mcp --proxy`\n")

        rows = []
        tot_full = tot_comp = 0
        for path in FILES:
            full = mcp.call("read_file", {"path": path})
            comp = mcp.call("read_file", {"path": path, "compress_bodies": True})
            bf, bc = len(full.encode()), len(comp.encode())
            tot_full += bf
            tot_comp += bc
            save = 100 * (1 - bc / bf) if bf else 0
            rows.append((path, bf, bc, save))

        name_w = max(len(p) for p, *_ in rows)
        print(f"{'file':<{name_w}}  {'full B':>9}  {'compress B':>11}  "
              f"{'full ~tok':>10}  {'compress ~tok':>13}  {'saved':>6}")
        print("-" * (name_w + 60))
        for path, bf, bc, save in rows:
            print(f"{path:<{name_w}}  {bf:>9,}  {bc:>11,}  "
                  f"{approx_tokens(bf):>10,}  {approx_tokens(bc):>13,}  {save:>5.0f}%")
        tot_save = 100 * (1 - tot_comp / tot_full) if tot_full else 0
        print("-" * (name_w + 60))
        print(f"{'TOTAL':<{name_w}}  {tot_full:>9,}  {tot_comp:>11,}  "
              f"{approx_tokens(tot_full):>10,}  {approx_tokens(tot_comp):>13,}  {tot_save:>5.0f}%")

        # Pattern 3: locate call sites instead of reading bodies at all.
        # Cost scales with match count, so the honest figure is per-match.
        st = mcp.call("search_text", {"query": SEARCH_QUERY, "limit": 100})
        try:
            n_matches = json.loads(st).get("count", 0)
        except json.JSONDecodeError:
            n_matches = st.count("path:")
        bs = len(st.encode())
        per = approx_tokens(bs) / n_matches if n_matches else 0
        print(f"\nsearch_text(query={SEARCH_QUERY!r}): {n_matches} sites located in "
              f"{bs:,} B (~{approx_tokens(bs):,} tok ≈ {per:.0f} tok/site) — "
              f"line-precise file:line, zero bodies read")

        print("\nVerdict inputs:")
        print(f"  • Full reads cost ~{approx_tokens(tot_full):,} tok for {len(FILES)} files.")
        print(f"  • compress_bodies:true would cost ~{approx_tokens(tot_comp):,} tok "
              f"({tot_save:.0f}% less) — same signatures/structure.")
        print(f"  • read_file's DEFAULT is compress_bodies:false → the expensive "
              f"path is the default path.")
    finally:
        mcp.close()


if __name__ == "__main__":
    main()

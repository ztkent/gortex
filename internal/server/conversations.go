package server

import (
	"net/http"

	"github.com/zzet/gortex/internal/llm/conversationlog"
)

// Conversation-log inspection routes. They expose the opt-in
// conversation-log sink (raw LLM request/response JSONL per session) to
// a local inspector:
//
//	GET /v1/conversations              list recorded sessions
//	GET /v1/conversations/{session}    per-file/phase request/response steps + usage
//	GET /v1/conversations/ui           a minimal self-contained HTML inspector
//
// Because these routes egress raw LLM I/O, each one applies the
// route-scoped DNS-rebind guard (guardConversationRoute) before
// responding. The guard cooperates with the existing --http-auth-token
// model: it allows loopback / allowlisted hosts OR a valid token. It is
// deliberately NOT installed in ServeHTTP, so no other route is affected
// and a token-authed non-loopback dashboard keeps working.

// SetConversationDir enables the /v1/conversations* routes by pointing
// them at the conversation-log directory. An empty dir leaves the routes
// mounted but reporting no sessions (the sink is off).
func (h *Handler) SetConversationDir(dir string) { h.convDir = dir }

// SetConversationGuard wires the route-scoped DNS-rebind guard for the
// conversation routes: an extra Host allowlist (beyond loopback) and the
// auth-token source so a valid token-authed request passes. tokenFn may
// be nil (no token configured); allow may be empty (loopback-only).
func (h *Handler) SetConversationGuard(allow []string, tokenFn func() string) {
	h.convAllow = allow
	h.convTokenFn = tokenFn
}

// conversationTokenOK reports whether the request carries a valid auth
// token, matching the source-of-truth check in WithAuthFunc (Bearer
// header or ?token=). When no token is configured, there is nothing to
// present, so this returns false and the guard falls back to the
// loopback/allowlist check.
func (h *Handler) conversationTokenOK(r *http.Request) bool {
	if h.convTokenFn == nil {
		return false
	}
	expected := h.convTokenFn()
	if expected == "" {
		return false
	}
	if authMatches([]byte(r.Header.Get("Authorization")), []byte("Bearer "+expected)) {
		return true
	}
	if q := r.URL.Query().Get("token"); q != "" && tokenMatches([]byte(q), []byte(expected)) {
		return true
	}
	return false
}

// guardConversation runs the route-scoped guard for one request and
// writes the 403 response when it fails. Returns true when the request
// may proceed.
func (h *Handler) guardConversation(w http.ResponseWriter, r *http.Request) bool {
	if guardConversationRoute(r, h.convAllow, h.conversationTokenOK(r)) {
		return true
	}
	WriteJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden host"})
	return false
}

// conversationReader returns a reader over the configured conversation
// directory (which may be empty, yielding an empty session list).
func (h *Handler) conversationReader() *conversationlog.Reader {
	return conversationlog.NewReader(h.convDir)
}

// --- GET /v1/conversations ---

func (h *Handler) handleConversations(w http.ResponseWriter, r *http.Request) {
	if !h.guardConversation(w, r) {
		return
	}
	sessions, err := h.conversationReader().ListSessions()
	if err != nil {
		WriteJSONError(w, http.StatusInternalServerError, "failed to read conversation sessions")
		return
	}
	if sessions == nil {
		sessions = []conversationlog.SessionSummary{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// --- GET /v1/conversations/{session} ---

func (h *Handler) handleConversationSession(w http.ResponseWriter, r *http.Request) {
	if !h.guardConversation(w, r) {
		return
	}
	session := r.PathValue("session")
	if session == "" {
		WriteJSONError(w, http.StatusBadRequest, "missing session id")
		return
	}
	recs, err := h.conversationReader().LoadSession(session)
	if err != nil {
		WriteJSONError(w, http.StatusInternalServerError, "failed to load session")
		return
	}
	total := len(recs)

	// Optional ?file= / ?phase= filters narrow the records to one
	// file/phase, mirroring the /v1/activity filter idiom.
	fileFilter := r.URL.Query().Get("file")
	phaseFilter := r.URL.Query().Get("phase")
	filtered := recs
	if fileFilter != "" || phaseFilter != "" {
		filtered = filtered[:0:0]
		for _, rec := range recs {
			if fileFilter != "" && rec.File != fileFilter {
				continue
			}
			if phaseFilter != "" && rec.Phase != phaseFilter {
				continue
			}
			filtered = append(filtered, rec)
		}
	}
	if filtered == nil {
		filtered = []conversationlog.Record{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"session":  session,
		"records":  filtered,
		"total":    total,
		"filtered": len(filtered),
	})
}

// --- GET /v1/conversations/ui ---

func (h *Handler) handleConversationsUI(w http.ResponseWriter, r *http.Request) {
	if !h.guardConversation(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(conversationInspectorHTML))
}

// conversationInspectorHTML is a minimal, self-contained inspector page:
// it lists sessions, lets you pick one, and renders each turn's request
// messages, response, and token usage. No external assets — it talks to
// the same-origin /v1/conversations JSON routes.
const conversationInspectorHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Gortex — Conversation Inspector</title>
<style>
  body { font: 14px/1.5 -apple-system, system-ui, sans-serif; margin: 0; color: #1d2127; background: #f6f8fa; }
  header { padding: 12px 16px; background: #24292f; color: #fff; }
  header h1 { margin: 0; font-size: 16px; }
  main { display: flex; height: calc(100vh - 45px); }
  #sessions { width: 280px; overflow-y: auto; border-right: 1px solid #d0d7de; background: #fff; }
  #sessions ul { list-style: none; margin: 0; padding: 0; }
  #sessions li { padding: 10px 14px; cursor: pointer; border-bottom: 1px solid #eaeef2; }
  #sessions li:hover { background: #f3f4f6; }
  #sessions li.active { background: #ddf4ff; }
  #sessions .meta { color: #57606a; font-size: 12px; }
  #detail { flex: 1; overflow-y: auto; padding: 16px; }
  .turn { background: #fff; border: 1px solid #d0d7de; border-radius: 6px; margin-bottom: 14px; padding: 12px; }
  .turn .hd { font-size: 12px; color: #57606a; margin-bottom: 8px; }
  .turn .tag { display: inline-block; background: #eaeef2; border-radius: 10px; padding: 1px 8px; margin-right: 6px; }
  .msg { margin: 6px 0; }
  .msg .role { font-weight: 600; color: #0969da; }
  pre { white-space: pre-wrap; word-break: break-word; background: #f6f8fa; border-radius: 4px; padding: 8px; margin: 4px 0; }
  .usage { font-size: 12px; color: #57606a; margin-top: 6px; }
  .est { color: #9a6700; }
  .empty { color: #57606a; padding: 24px; }
</style>
</head>
<body>
<header><h1>Gortex — Conversation Inspector</h1></header>
<main>
  <div id="sessions"><div class="empty">Loading…</div></div>
  <div id="detail"><div class="empty">Select a session.</div></div>
</main>
<script>
const esc = (s) => String(s == null ? "" : s).replace(/[&<>]/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;"}[c]));
async function loadSessions() {
  const r = await fetch("/v1/conversations");
  const j = await r.json();
  const el = document.getElementById("sessions");
  const list = (j.sessions || []);
  if (!list.length) { el.innerHTML = '<div class="empty">No recorded sessions.</div>'; return; }
  const ul = document.createElement("ul");
  for (const s of list) {
    const li = document.createElement("li");
    li.innerHTML = '<div>' + esc(s.session) + '</div>' +
      '<div class="meta">' + (s.records||0) + ' turns · ' + ((s.files||[]).length) + ' files</div>';
    li.onclick = () => { document.querySelectorAll("#sessions li").forEach(n => n.classList.remove("active")); li.classList.add("active"); loadSession(s.session); };
    ul.appendChild(li);
  }
  el.innerHTML = ""; el.appendChild(ul);
}
async function loadSession(session) {
  const r = await fetch("/v1/conversations/" + encodeURIComponent(session));
  const j = await r.json();
  const el = document.getElementById("detail");
  const recs = j.records || [];
  if (!recs.length) { el.innerHTML = '<div class="empty">No turns in this session.</div>'; return; }
  el.innerHTML = "";
  for (const rec of recs) {
    const d = document.createElement("div"); d.className = "turn";
    let h = '<div class="hd">';
    if (rec.file)  h += '<span class="tag">file: ' + esc(rec.file) + '</span>';
    if (rec.phase) h += '<span class="tag">phase: ' + esc(rec.phase) + '</span>';
    if (rec.provider) h += '<span class="tag">' + esc(rec.provider) + '</span>';
    if (rec.model) h += '<span class="tag">' + esc(rec.model) + '</span>';
    h += '</div>';
    for (const m of (rec.request || [])) {
      h += '<div class="msg"><span class="role">' + esc(m.Role || m.role) + '</span><pre>' + esc(m.Content || m.content) + '</pre></div>';
    }
    h += '<div class="msg"><span class="role">response</span><pre>' + esc(rec.response) + '</pre></div>';
    const est = rec.estimated ? ' <span class="est">(estimated)</span>' : '';
    h += '<div class="usage">in ' + (rec.input_tokens||0) + ' · out ' + (rec.output_tokens||0) + est + ' · ' + (rec.elapsed_ms||0) + 'ms</div>';
    if (rec.error) h += '<div class="usage est">error: ' + esc(rec.error) + '</div>';
    d.innerHTML = h; el.appendChild(d);
  }
}
loadSessions();
</script>
</body>
</html>
`

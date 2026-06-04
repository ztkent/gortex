package hooks

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// UserPromptSubmitInput is the JSON Claude Code sends on UserPromptSubmit. We
// only consume the fields we use; unknown fields are ignored.
type UserPromptSubmitInput struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	Prompt        string `json:"prompt"`
}

// userPromptProbeTimeout bounds the pre-turn context probe. It runs on every
// user turn, so it must be fast and must never block the turn.
const userPromptProbeTimeout = 800 * time.Millisecond

// maxInjectedHits caps how many relevant symbols are injected per turn so the
// block stays a nudge, not a wall of text.
const maxInjectedHits = 6

// runUserPromptSubmit handles a UserPromptSubmit hook: it proactively searches
// the graph for symbols relevant to the user's prompt and injects them as
// additionalContext, so the model reaches for Gortex's knowledge instead of
// blindly grepping. It is best-effort — any miss (daemon down, no hits, a
// trivial / non-code prompt) is a silent no-op so the turn is never blocked or
// polluted, and no warning is emitted (SessionStart already warns once when the
// daemon is down; doing so every turn would be noise).
func runUserPromptSubmit(data []byte) {
	var input UserPromptSubmitInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "UserPromptSubmit" {
		return
	}
	query := promptQuery(input.Prompt)
	if query == "" {
		return
	}
	hits, err := probeViaDaemon(query, userPromptProbeTimeout)
	if err != nil || len(hits) == 0 {
		return
	}
	block := buildPromptInjection(hits)
	if block == "" {
		return
	}
	out, err := json.Marshal(HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:     "UserPromptSubmit",
			AdditionalContext: block,
		},
	})
	if err != nil {
		return
	}
	fmt.Print(string(out))
}

// promptQuery normalizes a raw prompt into a search query, or "" when the
// prompt is too trivial / not code-related to bother probing. Slash commands
// and one-word acknowledgements ("ok", "go on") are skipped.
func promptQuery(prompt string) string {
	p := strings.TrimSpace(prompt)
	if p == "" || strings.HasPrefix(p, "/") {
		return ""
	}
	p = strings.Join(strings.Fields(p), " ") // collapse newlines / runs of spaces
	if r := []rune(p); len(r) > 240 {
		p = strings.TrimSpace(string(r[:240]))
	}
	// Require at least one token of length >= 3 so "ok", "yes" don't probe.
	for _, w := range strings.Fields(p) {
		if len(w) >= 3 {
			return p
		}
	}
	return ""
}

// buildPromptInjection renders the additionalContext block from search hits.
func buildPromptInjection(hits []grepSymbolHit) string {
	if len(hits) == 0 {
		return ""
	}
	if len(hits) > maxInjectedHits {
		hits = hits[:maxInjectedHits]
	}
	var sb strings.Builder
	sb.WriteString("## Gortex — relevant indexed symbols for your request\n\n")
	sb.WriteString("Before reaching for grep/Read, these graph symbols look relevant to what you just asked:\n\n")
	for _, h := range hits {
		kind := h.Kind
		if kind == "" {
			kind = "symbol"
		}
		if h.Line > 0 {
			fmt.Fprintf(&sb, "- `%s` (%s) — %s:%d\n", h.Name, kind, h.FilePath, h.Line)
		} else {
			fmt.Fprintf(&sb, "- `%s` (%s) — %s\n", h.Name, kind, h.FilePath)
		}
	}
	sb.WriteString("\nRead any of them with `get_symbol_source`, trace with `find_usages` / `get_callers`, " +
		"or call `smart_context` for the full working set. These are indexed graph facts — prefer them over grep/Read.\n")
	return sb.String()
}

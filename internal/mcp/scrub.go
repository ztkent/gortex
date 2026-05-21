package mcp

import (
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// ansiEscape matches ANSI / VT escape sequences: CSI (`ESC [ … final`),
// OSC (`ESC ] … terminator`), and the two-byte `ESC <Fe>` escapes.
// Stripping the whole sequence — not just the lone ESC byte — avoids
// leaving a `[31m`-style fragment behind in the text.
var ansiEscape = regexp.MustCompile(
	"\x1b\\[[0-?]*[ -/]*[@-~]" + // CSI
		"|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)" + // OSC … BEL / ST
		"|\x1b[@-_]") // other two-byte escapes

// scrubControlChars removes content that can corrupt how an MCP client
// renders the tool surface or smuggle terminal-control sequences into a
// client that echoes descriptions to a TTY: ANSI / VT escape sequences
// and C0 / C1 control characters. Tab and newline are kept — they are
// legitimate in a multi-line tool description. Returns s unchanged when
// it carries nothing to scrub (the common case for Gortex's static
// description literals).
func scrubControlChars(s string) string {
	if s == "" {
		return s
	}
	cleaned := ansiEscape.ReplaceAllString(s, "")
	var b strings.Builder
	b.Grow(len(cleaned))
	dropped := false
	for _, r := range cleaned {
		switch {
		case r == '\t' || r == '\n':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// C0 control, DEL, or C1 control — drop it.
			dropped = true
		default:
			b.WriteRune(r)
		}
	}
	if !dropped && cleaned == s {
		return s
	}
	return b.String()
}

// scrubToolText sanitizes a tool's human-facing text in place — its
// title, description, and every input-parameter description — before
// the tool is registered. Defense-in-depth: Gortex's tool text is
// static Go source, so in practice this is a no-op, but routing every
// registration through it guarantees no control sequence can ever
// reach a client's tools/list rendering, regardless of where a future
// description string originates.
//
// Pipe characters are deliberately NOT stripped: Gortex tool
// descriptions use `|` heavily and legitimately (enum lists like
// "json | gcx | toon"), and a pipe carries no corruption or injection
// risk on its own.
func scrubToolText(tool *mcp.Tool) {
	tool.Title = scrubControlChars(tool.Title)
	tool.Description = scrubControlChars(tool.Description)
	for _, prop := range tool.InputSchema.Properties {
		m, ok := prop.(map[string]any)
		if !ok {
			continue
		}
		if d, ok := m["description"].(string); ok {
			m["description"] = scrubControlChars(d)
		}
	}
}

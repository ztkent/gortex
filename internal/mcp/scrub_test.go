package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestScrubControlChars(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text unchanged", "a normal description", "a normal description"},
		{"pipes preserved", "Output format: json | gcx | toon", "Output format: json | gcx | toon"},
		{"multibyte runes preserved", "café — résumé — déjà", "café — résumé — déjà"},
		{"newline and tab kept", "line one\n\tindented", "line one\n\tindented"},
		{"NUL dropped", "before\x00after", "beforeafter"},
		{"carriage return dropped", "overwrite\rme", "overwriteme"},
		{"DEL dropped", "a\x7fb", "ab"},
		{"C1 control dropped", "ab", "ab"},
		{"ANSI color stripped", "\x1b[31mred\x1b[0m text", "red text"},
		{"ANSI cursor-move stripped", "x\x1b[2Ky", "xy"},
		{"bare ESC sequence stripped", "a\x1bMb", "ab"},
		{"empty string", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scrubControlChars(c.in); got != c.want {
				t.Errorf("scrubControlChars(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestScrubToolText(t *testing.T) {
	tool := mcp.NewTool("demo",
		mcp.WithDescription("does a thing\x1b[31m with color\x00 and a NUL"),
		mcp.WithString("arg", mcp.Description("a param\x07 with a bell")),
	)
	scrubToolText(&tool)

	if strings.ContainsAny(tool.Description, "\x00\x1b\x07") {
		t.Errorf("tool description still carries control chars: %q", tool.Description)
	}
	if !strings.Contains(tool.Description, "does a thing") {
		t.Errorf("scrub destroyed legitimate text: %q", tool.Description)
	}
	prop, ok := tool.InputSchema.Properties["arg"].(map[string]any)
	if !ok {
		t.Fatalf("arg property missing or wrong shape: %#v", tool.InputSchema.Properties["arg"])
	}
	desc, _ := prop["description"].(string)
	if strings.ContainsRune(desc, '\x07') {
		t.Errorf("param description still carries a control char: %q", desc)
	}
	if !strings.Contains(desc, "a param") {
		t.Errorf("scrub destroyed legitimate param text: %q", desc)
	}
}

// TestAddTool_ScrubsRegisteredDescription proves the scrub runs on the
// real registration path: a tool registered with a poisoned
// description lands in tools/list clean.
func TestAddTool_ScrubsRegisteredDescription(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "0")
	srv := newFullTestServer(t)
	srv.addTool(
		mcp.NewTool("scrub_probe", mcp.WithDescription("poisoned\x1b[31m\x00description")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		},
	)
	st, ok := srv.mcpServer.ListTools()["scrub_probe"]
	if !ok {
		t.Fatal("scrub_probe was not registered")
	}
	if strings.ContainsAny(st.Tool.Description, "\x00\x1b") {
		t.Errorf("registered description not scrubbed: %q", st.Tool.Description)
	}
}

package lint

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLanguageForPath(t *testing.T) {
	require.Equal(t, "go", LanguageForPath("a/b/main.go"))
	require.Equal(t, "python", LanguageForPath("x.py"))
	require.Equal(t, "shell", LanguageForPath("deploy.sh"))
	require.Equal(t, "shell", LanguageForPath("X.BASH")) // case-insensitive
	require.Equal(t, "", LanguageForPath("notes.txt"))
}

func TestParseGCCFormat(t *testing.T) {
	out := "main.go:3:5: expected '}', found EOF\nmain.go:1: bad\n\n"
	diags := parseGCCFormat([]byte(out), "gofmt")
	require.Len(t, diags, 2)
	require.Equal(t, 3, diags[0].Line)
	require.Equal(t, 5, diags[0].Column)
	require.Equal(t, SeverityError, diags[0].Severity)
	require.Equal(t, "expected '}', found EOF", diags[0].Message)
	require.Equal(t, "gofmt", diags[0].Linter)
	require.Equal(t, 1, diags[1].Line)
	require.Equal(t, 0, diags[1].Column, "missing column parses as 0")
}

func TestParseShellcheckGCC(t *testing.T) {
	out := "deploy.sh:2:1: warning: x is referenced but not assigned [SC2154]\n" +
		"deploy.sh:5:3: error: syntax error [SC1009]\n"
	diags := parseShellcheckGCC([]byte(out), "shellcheck")
	require.Len(t, diags, 2)
	require.Equal(t, SeverityWarning, diags[0].Severity)
	require.Equal(t, "SC2154", diags[0].Rule)
	require.Equal(t, 2, diags[0].Line)
	require.Equal(t, SeverityError, diags[1].Severity)
	require.Equal(t, "SC1009", diags[1].Rule)
}

func TestParseRuff(t *testing.T) {
	out := `[
	  {"code":"F401","message":"unused import","filename":"a.py","location":{"row":1,"column":8}},
	  {"code":null,"message":"SyntaxError: invalid syntax","filename":"a.py","location":{"row":3,"column":1}}
	]`
	diags := parseRuff([]byte(out), "ruff")
	require.Len(t, diags, 2)
	require.Equal(t, SeverityWarning, diags[0].Severity)
	require.Equal(t, "F401", diags[0].Rule)
	require.Equal(t, SeverityError, diags[1].Severity, "a null-code SyntaxError is an error")
	require.Empty(t, parseRuff([]byte("  "), "ruff"))
}

func TestRunSkipsMissingLinter(t *testing.T) {
	reg := NewRegistry(Linter{
		Name: "ghostlinter", Languages: []string{"go"},
		Bin: "gortex-nonexistent-linter-xyz", Args: []string{"{file}"}, parser: parseGCC,
	})
	res := reg.Run(context.Background(), "/tmp/x.go", "go", time.Second)
	require.Empty(t, res.Ran)
	require.Len(t, res.Skipped, 1)
	require.Equal(t, "ghostlinter", res.Skipped[0].Linter)
	require.Contains(t, res.Skipped[0].Reason, "not installed")
}

// TestRunGofmtEndToEnd exercises a real gofmt invocation on a broken and a
// clean Go file. Skips when gofmt is not on PATH.
func TestRunGofmtEndToEnd(t *testing.T) {
	reg := NewRegistry()
	gofmt := reg.ForLanguage("go")
	require.NotEmpty(t, gofmt)
	if !gofmt[0].Available() {
		t.Skip("gofmt not on PATH")
	}

	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.go")
	require.NoError(t, os.WriteFile(broken, []byte("package main\n\nfunc main() {\n"), 0o644))
	res := reg.Run(context.Background(), broken, "go", 5*time.Second)
	require.Contains(t, res.Ran, "gofmt")
	require.NotEmpty(t, res.Diagnostics, "gofmt should report the missing brace")
	require.Equal(t, SeverityError, res.Diagnostics[0].Severity)

	clean := filepath.Join(dir, "clean.go")
	require.NoError(t, os.WriteFile(clean, []byte("package main\n\nfunc main() {}\n"), 0o644))
	res = reg.Run(context.Background(), clean, "go", 5*time.Second)
	require.Contains(t, res.Ran, "gofmt")
	require.Empty(t, res.Diagnostics, "a clean file yields no diagnostics")
}

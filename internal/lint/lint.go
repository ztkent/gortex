// Package lint runs external language linters/formatters against a single file
// and normalizes their output into structured diagnostics. It is the engine
// behind the lint_file MCP tool: an LSP-free, on-demand syntax/lint check that
// complements the LSP-backed diagnostics path. A linter that is not installed
// is reported as skipped — never as a hard error — so the bridge degrades
// gracefully on machines that lack a given tool.
package lint

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Severity is the normalized severity of a diagnostic across linters.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// Diagnostic is one normalized linter finding.
type Diagnostic struct {
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Column   int      `json:"column,omitempty"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Rule     string   `json:"rule,omitempty"`
	Linter   string   `json:"linter"`
}

// parserKind selects how a linter's output is normalized.
type parserKind int

const (
	parseGCC       parserKind = iota // file:line[:col]: message
	parseShellcheck                  // file:line:col: severity: message [SCxxxx]
	parseRuffJSON                    // ruff --output-format=json
)

// Linter describes one external tool invocation.
type Linter struct {
	Name      string
	Languages []string
	Bin       string
	// Args is the command line; the literal token "{file}" is replaced with
	// the absolute path of the file being linted.
	Args      []string
	parser    parserKind
	useStderr bool // parse stderr instead of stdout (gofmt prints errors there)
}

// Available reports whether the linter's binary is on PATH.
func (l Linter) Available() bool {
	_, err := exec.LookPath(l.Bin)
	return err == nil
}

// DefaultLinters returns the built-in linter set. Each entry runs read-only
// and reports diagnostics without modifying the file.
func DefaultLinters() []Linter {
	return []Linter{
		// gofmt -e parses the file and prints any syntax error to stderr as
		// `file:line:col: message`; the reformatted source on stdout is ignored.
		{Name: "gofmt", Languages: []string{"go"}, Bin: "gofmt", Args: []string{"-e", "{file}"}, parser: parseGCC, useStderr: true},
		{Name: "shellcheck", Languages: []string{"shell"}, Bin: "shellcheck", Args: []string{"-f", "gcc", "{file}"}, parser: parseShellcheck},
		{Name: "ruff", Languages: []string{"python"}, Bin: "ruff", Args: []string{"check", "--output-format=json", "{file}"}, parser: parseRuffJSON},
	}
}

// Skipped records a linter that did not run and why.
type Skipped struct {
	Linter string `json:"linter"`
	Reason string `json:"reason"`
}

// Result is the outcome of linting one file.
type Result struct {
	Language    string       `json:"language"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	Ran         []string     `json:"linters_ran"`
	Skipped     []Skipped    `json:"linters_skipped"`
}

// Registry holds the linters available to Run.
type Registry struct {
	linters []Linter
}

// NewRegistry builds a registry from the given linters, or the built-in set
// when none are supplied. Passing custom linters is the extension point for
// config-defined tools.
func NewRegistry(linters ...Linter) *Registry {
	if len(linters) == 0 {
		linters = DefaultLinters()
	}
	return &Registry{linters: linters}
}

const defaultTimeout = 5 * time.Second

// ForLanguage returns the registered linters that target lang.
func (r *Registry) ForLanguage(lang string) []Linter {
	var out []Linter
	for _, l := range r.linters {
		for _, lg := range l.Languages {
			if lg == lang {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

// Run executes every registered linter for lang against absPath and returns
// normalized diagnostics. A linter that is not installed, or that fails to
// start / times out, is recorded in Skipped — it never fails the call. A
// non-zero exit (the normal signal that a linter found issues) is not an
// error. timeout <= 0 uses the default.
func (r *Registry) Run(ctx context.Context, absPath, lang string, timeout time.Duration) Result {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	res := Result{Language: lang, Diagnostics: []Diagnostic{}, Ran: []string{}, Skipped: []Skipped{}}
	for _, l := range r.ForLanguage(lang) {
		if !l.Available() {
			res.Skipped = append(res.Skipped, Skipped{Linter: l.Name, Reason: "not installed (" + l.Bin + " not on PATH)"})
			continue
		}
		diags, err := runOne(ctx, l, absPath, timeout)
		if err != nil {
			res.Skipped = append(res.Skipped, Skipped{Linter: l.Name, Reason: err.Error()})
			continue
		}
		res.Ran = append(res.Ran, l.Name)
		res.Diagnostics = append(res.Diagnostics, diags...)
	}
	return res
}

func runOne(ctx context.Context, l Linter, absPath string, timeout time.Duration) ([]Diagnostic, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := make([]string, len(l.Args))
	for i, a := range l.Args {
		args[i] = strings.ReplaceAll(a, "{file}", absPath)
	}
	cmd := exec.CommandContext(cctx, l.Bin, args...)
	cmd.Dir = filepath.Dir(absPath) // so per-project linter config is discovered

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	if cctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("timed out after %s", timeout)
	}
	if runErr != nil {
		// A non-zero exit is how a linter signals it found issues — expected.
		// Only a genuine start failure (binary vanished, etc.) is an error.
		if _, ok := runErr.(*exec.ExitError); !ok {
			return nil, runErr
		}
	}

	switch l.parser {
	case parseRuffJSON:
		return parseRuff(stdout.Bytes(), l.Name), nil
	case parseShellcheck:
		return parseShellcheckGCC(stdout.Bytes(), l.Name), nil
	default:
		out := stdout.Bytes()
		if l.useStderr {
			out = stderr.Bytes()
		}
		return parseGCCFormat(out, l.Name), nil
	}
}

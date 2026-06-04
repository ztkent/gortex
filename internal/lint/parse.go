package lint

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// extLang maps file extensions to the linter languages this package supports.
var extLang = map[string]string{
	".go":   "go",
	".py":   "python",
	".pyi":  "python",
	".sh":   "shell",
	".bash": "shell",
	".zsh":  "shell",
	".ksh":  "shell",
}

// LanguageForPath infers the linter language from a file's extension, or "" if
// no built-in linter targets that type.
func LanguageForPath(p string) string {
	return extLang[strings.ToLower(filepath.Ext(p))]
}

// gccLineRe matches the `file:line[:col]: message` convention emitted by gofmt
// -e and many other compilers/linters.
var gccLineRe = regexp.MustCompile(`^(.*?):(\d+):(?:(\d+):)?\s*(.*)$`)

func parseGCCFormat(out []byte, linter string) []Diagnostic {
	var diags []Diagnostic
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := gccLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ln, _ := strconv.Atoi(m[2])
		col := 0
		if m[3] != "" {
			col, _ = strconv.Atoi(m[3])
		}
		diags = append(diags, Diagnostic{
			File:     m[1],
			Line:     ln,
			Column:   col,
			Severity: SeverityError,
			Message:  strings.TrimSpace(m[4]),
			Linter:   linter,
		})
	}
	return diags
}

// shellcheckRe matches shellcheck's `-f gcc` output:
// `file:line:col: severity: message [SCxxxx]`.
var shellcheckRe = regexp.MustCompile(`^(.*?):(\d+):(\d+):\s*(\w+):\s*(.*?)(?:\s*\[(SC\d+)\])?$`)

func parseShellcheckGCC(out []byte, linter string) []Diagnostic {
	var diags []Diagnostic
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		m := shellcheckRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ln, _ := strconv.Atoi(m[2])
		col, _ := strconv.Atoi(m[3])
		diags = append(diags, Diagnostic{
			File:     m[1],
			Line:     ln,
			Column:   col,
			Severity: shellcheckSeverity(m[4]),
			Message:  strings.TrimSpace(m[5]),
			Rule:     m[6],
			Linter:   linter,
		})
	}
	return diags
}

func shellcheckSeverity(s string) Severity {
	switch strings.ToLower(s) {
	case "error":
		return SeverityError
	case "warning":
		return SeverityWarning
	default: // note, style, info
		return SeverityInfo
	}
}

type ruffEntry struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Filename string `json:"filename"`
	Location struct {
		Row    int `json:"row"`
		Column int `json:"column"`
	} `json:"location"`
}

func parseRuff(out []byte, linter string) []Diagnostic {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil
	}
	var entries []ruffEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil
	}
	diags := make([]Diagnostic, 0, len(entries))
	for _, e := range entries {
		diags = append(diags, Diagnostic{
			File:     e.Filename,
			Line:     e.Location.Row,
			Column:   e.Location.Column,
			Severity: ruffSeverity(e.Code, e.Message),
			Message:  e.Message,
			Rule:     e.Code,
			Linter:   linter,
		})
	}
	return diags
}

// ruffSeverity treats syntax errors (E999, the E9 class, or a SyntaxError
// message — ruff emits these with a null code) as errors; everything else is a
// lint warning.
func ruffSeverity(code, message string) Severity {
	if code == "" || strings.HasPrefix(code, "E9") ||
		strings.Contains(strings.ToLower(message), "syntaxerror") {
		return SeverityError
	}
	return SeverityWarning
}

package mcp

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/excludes"
)

// omission builds one machine-readable omission note for a tool
// response envelope. kind is a stable token — compressed, truncated,
// binary, vendored, or generated — and detail is a short human
// explanation. The note tells the model that code is intentionally
// absent or reshaped, so it does not hallucinate about what it cannot
// see in the payload.
func omission(kind, detail string) map[string]any {
	return map[string]any{"kind": kind, "detail": detail}
}

// pathOmissions returns the vendored / generated notes that can be
// inferred from a file path alone. Both may apply; the result is nil
// when neither does.
func pathOmissions(path string) []map[string]any {
	var notes []map[string]any
	if excludes.IsVendored(path) {
		notes = append(notes, omission("vendored",
			"file lives under a vendored dependency or build-output directory — not first-party code"))
	}
	if isGeneratedFile(path) {
		notes = append(notes, omission("generated",
			"file name matches a code-generation convention — edits here are overwritten by the generator"))
	}
	return notes
}

// generatedSuffixes lists file-name suffixes that mark generated code
// across the languages Gortex indexes.
var generatedSuffixes = []string{
	".pb.go", ".pb.cc", ".pb.h", ".pb.swift", "_pb2.py", "_pb2_grpc.py",
	"_gen.go", ".gen.go", "_generated.go", ".generated.go",
	".g.dart", ".freezed.dart", ".g.cs", ".designer.cs",
}

// isGeneratedFile reports whether a file name matches a common
// code-generation convention.
func isGeneratedFile(path string) bool {
	base := filepath.Base(path)
	for _, suf := range generatedSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	if strings.HasPrefix(base, "zz_generated") {
		return true
	}
	if strings.HasSuffix(base, ".go") &&
		(strings.HasPrefix(base, "mock_") || strings.HasSuffix(base, "_mock.go")) {
		return true
	}
	return false
}

// looksBinary reports whether content is non-text. A NUL byte in the
// sampled head is the signal git and most editors use.
func looksBinary(content []byte) bool {
	n := len(content)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if content[i] == 0 {
			return true
		}
	}
	return false
}

// omissionKindsCSV joins the kind tokens of a note list for a GCX
// header, where the space-splitting header tokeniser rules out the
// prose detail. Returns "" for an empty list.
func omissionKindsCSV(notes []map[string]any) string {
	if len(notes) == 0 {
		return ""
	}
	kinds := make([]string, 0, len(notes))
	for _, n := range notes {
		if k, ok := n["kind"].(string); ok {
			kinds = append(kinds, k)
		}
	}
	return strings.Join(kinds, ",")
}

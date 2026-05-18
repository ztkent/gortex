package wiki

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
)

// FileResult describes one file the writer materialised on disk.
// It is returned to the caller in stable (path-sorted) order so
// tests and MCP responses are deterministic.
type FileResult struct {
	Path   string `json:"path"`   // absolute path
	Bytes  int    `json:"bytes"`  // content length in bytes
	SHA256 string `json:"sha256"` // hex digest, lower-case
}

// Writer collects in-memory file payloads, then materialises them on
// disk via Flush. Idempotent: it only writes when the new content
// differs from what's already on disk so re-running the wiki keeps
// mtimes stable for unchanged pages.
type Writer struct {
	root  string
	files map[string][]byte // relative path → contents
}

// NewWriter creates a writer rooted at outputDir. The root directory
// is created on Flush if it doesn't already exist.
func NewWriter(outputDir string) *Writer {
	return &Writer{
		root:  outputDir,
		files: make(map[string][]byte),
	}
}

// Write queues a file. Path is interpreted relative to the writer's
// root. Calling Write twice with the same path replaces the prior
// content — the wiki generator does this when a later page re-renders
// the same target.
func (w *Writer) Write(relPath string, content []byte) {
	w.files[filepath.ToSlash(relPath)] = content
}

// Flush materialises every queued file under root, creating
// intermediate directories as needed. Returns the manifest in
// path-sorted order.
func (w *Writer) Flush() ([]FileResult, error) {
	if err := os.MkdirAll(w.root, 0o755); err != nil {
		return nil, fmt.Errorf("create wiki root %q: %w", w.root, err)
	}

	// Sort paths so the manifest and the on-disk write order are
	// deterministic — useful for golden tests.
	paths := make([]string, 0, len(w.files))
	for p := range w.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var out []FileResult
	for _, rel := range paths {
		body := w.files[rel]
		abs := filepath.Join(w.root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %q: %w", filepath.Dir(abs), err)
		}
		// Idempotency: skip the write when on-disk bytes match.
		if existing, err := os.ReadFile(abs); err == nil && bytesEqual(existing, body) {
			out = append(out, FileResult{
				Path:   abs,
				Bytes:  len(body),
				SHA256: hashHex(body),
			})
			continue
		}
		if err := os.WriteFile(abs, body, 0o644); err != nil {
			return nil, fmt.Errorf("write %q: %w", abs, err)
		}
		out = append(out, FileResult{
			Path:   abs,
			Bytes:  len(body),
			SHA256: hashHex(body),
		})
	}
	return out, nil
}

// Files returns the queued (rel-path → content) payload — used by
// tests that prefer to assert against memory rather than disk.
func (w *Writer) Files() map[string][]byte {
	out := make(map[string][]byte, len(w.files))
	maps.Copy(out, w.files)
	return out
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

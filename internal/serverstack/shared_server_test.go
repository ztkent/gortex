package serverstack

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
)

// TestNewSharedServer_OneshotMemory asserts the shared constructor builds
// a working stack over a tmp repo: the graph indexes, and the engine /
// MCP server / overlay manager are wired. This is the single-construction-
// path validation.
func TestNewSharedServer_OneshotMemory(t *testing.T) {
	repo := t.TempDir()
	src := "package toy\n\nfunc Add(a, b int) int { return a + b }\nfunc Mul(a, b int) int { return a * b }\n"
	if err := os.WriteFile(filepath.Join(repo, "toy.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	ss, err := NewSharedServer(SharedServerConfig{
		Lifecycle:  LifecycleOneshot,
		Index:      repo,
		Config:     config.Default(),
		Logger:     zap.NewNop(),
		Version:    "test",
		SideStores: SideStores{NotesDir: t.TempDir(), NotesRepo: "test"},
		// Pin the savings ledger + legacy-import probe to temp paths:
		// with both empty the constructor opens the REAL machine-global
		// sidecar and imports (renaming!) the developer's real flat-file
		// ledger — a unit test must never mutate ~/.gortex.
		SavingsPath:       filepath.Join(t.TempDir(), "sidecar.sqlite"),
		SavingsLegacyJSON: filepath.Join(t.TempDir(), "savings.json"),
	})
	if err != nil {
		t.Fatalf("NewSharedServer: %v", err)
	}
	defer func() { _ = ss.Close() }()

	if ss.Graph == nil || ss.Indexer == nil || ss.Engine == nil || ss.MCP == nil {
		t.Fatalf("incomplete stack: graph=%v idx=%v eng=%v mcp=%v", ss.Graph != nil, ss.Indexer != nil, ss.Engine != nil, ss.MCP != nil)
	}
	if ss.Overlays == nil {
		t.Error("overlay manager should be wired")
	}

	if _, err := ss.Indexer.Index(repo); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if n := ss.Graph.Stats().TotalNodes; n == 0 {
		t.Fatal("graph should be non-empty after indexing the tmp repo")
	}
}

// TestNewSharedServer_OneshotDefaultsMemory asserts the oneshot lifecycle
// resolves to the memory backend when Backend is empty.
func TestNewSharedServer_OneshotDefaultsMemory(t *testing.T) {
	if got := LifecycleOneshot.defaultBackend(); got != "memory" {
		t.Errorf("oneshot default backend = %q, want memory", got)
	}
	if got := LifecycleDaemon.defaultBackend(); got != "sqlite" {
		t.Errorf("daemon default backend = %q, want sqlite", got)
	}
	if got := LifecycleHTTP.defaultBackend(); got != "sqlite" {
		t.Errorf("http default backend = %q, want sqlite", got)
	}
	if LifecycleOneshot.Writable() {
		t.Error("oneshot must not be writable (no store lock)")
	}
	if !LifecycleDaemon.Writable() || !LifecycleHTTP.Writable() {
		t.Error("daemon/http lifecycles own a durable store")
	}
}

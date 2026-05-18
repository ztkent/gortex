package wiki

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

// fakeProvider implements llm.Provider for deterministic tests.
type fakeProvider struct {
	name  string
	reply string
	err   error
	calls int
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Complete(_ context.Context, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
	f.calls++
	if f.err != nil {
		return llm.CompletionResponse{}, f.err
	}
	return llm.CompletionResponse{Text: f.reply}, nil
}
func (f *fakeProvider) Close() error { return nil }

func TestClaudeCLIEnhancer_CacheHitsAreByteIdentical(t *testing.T) {
	tmp := t.TempDir()
	cache := NewEnhanceCache(filepath.Join(tmp, "cache"))
	fp := &fakeProvider{name: "claudecli", reply: "ENHANCED BODY"}
	en := NewClaudeCLIEnhancer(fp, cache)

	section := EnhanceSection{
		Kind:        "community",
		PageTitle:   "Indexer",
		RawMarkdown: "## Files\n- a.go",
		Context:     "size=10",
	}
	first, err := en.Enhance(context.Background(), section)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if fp.calls != 1 {
		t.Errorf("first call should hit provider; calls=%d", fp.calls)
	}
	second, err := en.Enhance(context.Background(), section)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if fp.calls != 1 {
		t.Errorf("second call should hit cache; calls=%d", fp.calls)
	}
	if first != second {
		t.Errorf("cached output differs: %q vs %q", first, second)
	}
}

func TestClaudeCLIEnhancer_ProviderErrorFallsBack(t *testing.T) {
	tmp := t.TempDir()
	cache := NewEnhanceCache(filepath.Join(tmp, "cache"))
	fp := &fakeProvider{name: "claudecli", err: errors.New("boom")}
	en := NewClaudeCLIEnhancer(fp, cache)

	section := EnhanceSection{
		Kind:        "process",
		PageTitle:   "Process X",
		RawMarkdown: "ORIGINAL",
	}
	out, err := en.Enhance(context.Background(), section)
	if err == nil {
		t.Error("expected error to propagate")
	}
	if out != "ORIGINAL" {
		t.Errorf("on provider error, output should be the raw markdown; got %q", out)
	}
}

func TestClaudeCLIEnhancer_EmptyReplyKeepsOriginal(t *testing.T) {
	fp := &fakeProvider{name: "claudecli", reply: "   "}
	en := NewClaudeCLIEnhancer(fp, nil)
	out, err := en.Enhance(context.Background(), EnhanceSection{
		Kind:        "architecture",
		PageTitle:   "Arch",
		RawMarkdown: "RAW",
	})
	if err != nil {
		t.Fatalf("Enhance: %v", err)
	}
	if out != "RAW" {
		t.Errorf("empty reply should preserve raw; got %q", out)
	}
}

func TestClaudeCLIEnhancer_NoProviderIsNoop(t *testing.T) {
	en := NewClaudeCLIEnhancer(nil, nil)
	out, err := en.Enhance(context.Background(), EnhanceSection{
		Kind:        "community",
		PageTitle:   "X",
		RawMarkdown: "KEEP",
	})
	if err != nil {
		t.Fatalf("Enhance: %v", err)
	}
	if out != "KEEP" {
		t.Errorf("nil provider should preserve raw; got %q", out)
	}
}

func TestEnhanceCache_KeyVariesWithPromptVersion(t *testing.T) {
	cache := NewEnhanceCache("")
	s := EnhanceSection{Kind: "community", PageTitle: "X", RawMarkdown: "Y"}
	k1 := cache.Key(s, "claudecli")
	if k1 == "" {
		t.Fatal("Key returned empty")
	}
	// Same inputs → identical key.
	if k2 := cache.Key(s, "claudecli"); k1 != k2 {
		t.Errorf("same inputs should produce same key; got %q vs %q", k1, k2)
	}
	// Different provider → different key.
	if k3 := cache.Key(s, "ollama"); k1 == k3 {
		t.Errorf("provider should affect key")
	}
}

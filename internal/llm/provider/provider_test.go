package provider

import (
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

func TestNew_UnknownProvider(t *testing.T) {
	if _, err := New(llm.Config{Provider: "bogus"}.ApplyDefaults()); err == nil {
		t.Fatal("expected error for an unknown provider")
	}
}

func TestNew_AnthropicMissingKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := New(llm.Config{Provider: "anthropic"}.ApplyDefaults()); err == nil {
		t.Fatal("expected error when ANTHROPIC_API_KEY is unset")
	}
}

func TestNew_AnthropicOK(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	p, err := New(llm.Config{Provider: "anthropic"}.ApplyDefaults())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer p.Close()
	if p.Name() != "anthropic" {
		t.Errorf("Name()=%q want anthropic", p.Name())
	}
}

func TestNew_OpenAIOK(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	p, err := New(llm.Config{Provider: "openai"}.ApplyDefaults())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer p.Close()
	if p.Name() != "openai" {
		t.Errorf("Name()=%q want openai", p.Name())
	}
}

func TestNew_OllamaMissingModel(t *testing.T) {
	if _, err := New(llm.Config{Provider: "ollama"}.ApplyDefaults()); err == nil {
		t.Fatal("expected error when llm.ollama.model is unset")
	}
}

func TestNew_OllamaOK(t *testing.T) {
	cfg := llm.Config{Provider: "ollama", Ollama: llm.OllamaConfig{Model: "qwen2.5-coder:7b"}}.ApplyDefaults()
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer p.Close()
	if p.Name() != "ollama" {
		t.Errorf("Name()=%q want ollama", p.Name())
	}
}

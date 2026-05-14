package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/llm"
)

func TestLoadGlobal_LLMSectionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`active_project: ""
repos: []
llm:
    provider: local
    max_steps: 12
    local:
        model: /opt/models/qwen.gguf
        template: chatml
        ctx: 4096
        gpu_layers: 999
    anthropic:
        model: claude-sonnet-4-6
`), 0o644))

	gc, err := LoadGlobal(cfgPath)
	require.NoError(t, err)
	require.NotNil(t, gc)
	assert.Equal(t, "local", gc.LLM.Provider)
	assert.Equal(t, 12, gc.LLM.MaxSteps)
	assert.Equal(t, "/opt/models/qwen.gguf", gc.LLM.Local.Model)
	assert.Equal(t, "chatml", gc.LLM.Local.Template)
	assert.Equal(t, 4096, gc.LLM.Local.Ctx)
	assert.Equal(t, 999, gc.LLM.Local.GPULayers)
	assert.Equal(t, "claude-sonnet-4-6", gc.LLM.Anthropic.Model)
}

func TestGlobalConfig_MergeLLMInto_FillsZeroFields(t *testing.T) {
	gc := &GlobalConfig{LLM: llm.Config{
		Provider: "local",
		MaxSteps: 16,
		Local: llm.LocalConfig{
			Model:     "/global/qwen.gguf",
			Template:  "chatml",
			Ctx:       4096,
			GPULayers: 999,
		},
	}}

	got := gc.MergeLLMInto(llm.Config{})
	assert.Equal(t, "local", got.Provider)
	assert.Equal(t, 16, got.MaxSteps)
	assert.Equal(t, "/global/qwen.gguf", got.Local.Model)
	assert.Equal(t, "chatml", got.Local.Template)
	assert.Equal(t, 4096, got.Local.Ctx)
	assert.Equal(t, 999, got.Local.GPULayers)
}

func TestGlobalConfig_MergeLLMInto_LocalWinsPerField(t *testing.T) {
	gc := &GlobalConfig{LLM: llm.Config{
		Provider: "local",
		MaxSteps: 16,
		Local: llm.LocalConfig{
			Model:    "/global/qwen.gguf",
			Template: "chatml",
			Ctx:      4096,
		},
	}}

	got := gc.MergeLLMInto(llm.Config{
		Local: llm.LocalConfig{
			Model: "/repo/override.gguf", // local wins
			Ctx:   8192,                  // local wins
		},
	})
	assert.Equal(t, "/repo/override.gguf", got.Local.Model)
	assert.Equal(t, 8192, got.Local.Ctx)
	// Unset locals fall through to global.
	assert.Equal(t, "chatml", got.Local.Template)
	assert.Equal(t, 16, got.MaxSteps)
	assert.Equal(t, "local", got.Provider)
}

func TestGlobalConfig_MergeLLMInto_PerProviderSubBlocks(t *testing.T) {
	gc := &GlobalConfig{LLM: llm.Config{
		Anthropic: llm.RemoteConfig{Model: "claude-sonnet-4-6", APIKeyEnv: "ANTHROPIC_API_KEY"},
		Ollama:    llm.OllamaConfig{Host: "http://localhost:11434"},
	}}

	// Repo selects a different provider and overrides only one field.
	got := gc.MergeLLMInto(llm.Config{
		Provider:  "anthropic",
		Anthropic: llm.RemoteConfig{Model: "claude-opus-4-7"},
	})
	assert.Equal(t, "anthropic", got.Provider)
	assert.Equal(t, "claude-opus-4-7", got.Anthropic.Model)       // local wins
	assert.Equal(t, "ANTHROPIC_API_KEY", got.Anthropic.APIKeyEnv) // global fills
	assert.Equal(t, "http://localhost:11434", got.Ollama.Host)    // unrelated block still merges
}

func TestGlobalConfig_MergeLLMInto_NilReceiver(t *testing.T) {
	var gc *GlobalConfig // nil
	local := llm.Config{Local: llm.LocalConfig{Model: "/repo/x.gguf"}}
	got := gc.MergeLLMInto(local)
	assert.Equal(t, "/repo/x.gguf", got.Local.Model)
}

func TestGlobalConfig_MergeLLMInto_ExpandsHomeInModelPath(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	gc := &GlobalConfig{LLM: llm.Config{Local: llm.LocalConfig{Model: "~/models/qwen.gguf"}}}
	got := gc.MergeLLMInto(llm.Config{})
	assert.Equal(t, filepath.Join(home, "models/qwen.gguf"), got.Local.Model)

	// Local override also gets expanded.
	got = gc.MergeLLMInto(llm.Config{Local: llm.LocalConfig{Model: "~/repo-override.gguf"}})
	assert.Equal(t, filepath.Join(home, "repo-override.gguf"), got.Local.Model)
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		{"~", home},
		{"~/models/foo.gguf", filepath.Join(home, "models/foo.gguf")},
		{"~weird", "~weird"}, // only `~/` form is expanded
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, expandHome(tc.in), "in=%q", tc.in)
	}
}

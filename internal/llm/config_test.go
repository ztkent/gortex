package llm

import "testing"

func TestConfig_ProviderName_DefaultsToLocal(t *testing.T) {
	if got := (Config{}).ProviderName(); got != "local" {
		t.Fatalf("empty provider: got %q want local", got)
	}
	if got := (Config{Provider: "  Anthropic "}).ProviderName(); got != "anthropic" {
		t.Fatalf("normalisation: got %q want anthropic", got)
	}
}

func TestConfig_IsEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"empty", Config{}, false},
		{"local with model", Config{Provider: "local", Local: LocalConfig{Model: "/m.gguf"}}, true},
		{"local no model", Config{Provider: "local"}, false},
		{"anthropic with model", Config{Provider: "anthropic", Anthropic: RemoteConfig{Model: "claude"}}, true},
		{"anthropic no model", Config{Provider: "anthropic"}, false},
		{"openai with model", Config{Provider: "openai", OpenAI: RemoteConfig{Model: "gpt"}}, true},
		{"ollama with model", Config{Provider: "ollama", Ollama: OllamaConfig{Model: "qwen"}}, true},
		{"ollama no model", Config{Provider: "ollama"}, false},
		{"unknown provider", Config{Provider: "bogus", Local: LocalConfig{Model: "/m.gguf"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.IsEnabled(); got != tc.want {
				t.Fatalf("IsEnabled=%v want %v", got, tc.want)
			}
		})
	}
}

func TestConfig_ApplyDefaults(t *testing.T) {
	c := Config{}.ApplyDefaults()
	if c.Provider != "local" {
		t.Errorf("provider=%q want local", c.Provider)
	}
	if c.MaxSteps != 16 {
		t.Errorf("max_steps=%d want 16", c.MaxSteps)
	}
	if c.Local.Ctx != 4096 || c.Local.GPULayers != 999 || c.Local.Template != "chatml" {
		t.Errorf("local defaults wrong: %+v", c.Local)
	}
	if c.Anthropic.Model != defaultAnthropicModel || c.Anthropic.APIKeyEnv != defaultAnthropicKeyEnv || c.Anthropic.BaseURL != defaultAnthropicBaseURL {
		t.Errorf("anthropic defaults wrong: %+v", c.Anthropic)
	}
	if c.OpenAI.Model != defaultOpenAIModel || c.OpenAI.APIKeyEnv != defaultOpenAIKeyEnv || c.OpenAI.BaseURL != defaultOpenAIBaseURL {
		t.Errorf("openai defaults wrong: %+v", c.OpenAI)
	}
	if c.Ollama.Host != defaultOllamaHost {
		t.Errorf("ollama host=%q want %q", c.Ollama.Host, defaultOllamaHost)
	}
}

func TestConfig_ApplyDefaults_Idempotent(t *testing.T) {
	once := Config{Provider: "anthropic", Anthropic: RemoteConfig{Model: "m"}}.ApplyDefaults()
	twice := once.ApplyDefaults()
	if once != twice {
		t.Fatalf("ApplyDefaults not idempotent:\n once=%+v\n twice=%+v", once, twice)
	}
}

func TestConfig_MergeEnv(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "anthropic")
	t.Setenv("GORTEX_LLM_MODEL", "claude-opus-4-7")
	t.Setenv("GORTEX_LLM_MAX_STEPS", "8")
	c := Config{}.MergeEnv()
	if c.Provider != "anthropic" {
		t.Errorf("provider=%q want anthropic", c.Provider)
	}
	if c.Anthropic.Model != "claude-opus-4-7" {
		t.Errorf("anthropic model=%q — GORTEX_LLM_MODEL should target the active provider", c.Anthropic.Model)
	}
	if c.MaxSteps != 8 {
		t.Errorf("max_steps=%d want 8", c.MaxSteps)
	}
}

func TestConfig_MergeEnv_ModelTargetsLocalByDefault(t *testing.T) {
	t.Setenv("GORTEX_LLM_PROVIDER", "")
	t.Setenv("GORTEX_LLM_MODEL", "/local/m.gguf")
	c := Config{}.MergeEnv()
	if c.Local.Model != "/local/m.gguf" {
		t.Errorf("local model=%q want /local/m.gguf", c.Local.Model)
	}
}

func TestConfig_MergedWith(t *testing.T) {
	global := Config{
		Provider:  "local",
		MaxSteps:  16,
		Local:     LocalConfig{Model: "/g.gguf", Template: "chatml", Ctx: 4096},
		Anthropic: RemoteConfig{APIKeyEnv: "ANTHROPIC_API_KEY"},
	}
	local := Config{Local: LocalConfig{Model: "/repo.gguf"}} // overrides only the model

	got := local.MergedWith(global)
	if got.Provider != "local" {
		t.Errorf("provider=%q — global should fill", got.Provider)
	}
	if got.Local.Model != "/repo.gguf" {
		t.Errorf("local model=%q — repo should win", got.Local.Model)
	}
	if got.Local.Template != "chatml" || got.Local.Ctx != 4096 {
		t.Errorf("local sub-fields not filled from global: %+v", got.Local)
	}
	if got.Anthropic.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("anthropic block not merged: %+v", got.Anthropic)
	}
	if got.MaxSteps != 16 {
		t.Errorf("max_steps=%d — global should fill", got.MaxSteps)
	}
}

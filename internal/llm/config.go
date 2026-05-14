// Package llm — config loader for the LLM service.
//
// This file is pure Go (no build tag) so every build can compile it.
// The actual provider construction lives under internal/llm/provider/
// — the `local` provider is the only one that needs `-tags llama`.
//
// Resolution order: file values are populated by the gortex config
// loader; MergeEnv overlays any GORTEX_LLM_* env var that's set (env
// wins); ApplyDefaults fills any remaining zero fields. A repo-local
// Config can additionally be layered over a global one via MergedWith.
package llm

import (
	"os"
	"strconv"
	"strings"
)

// Config is the YAML-friendly `llm:` block. The active backend is
// chosen by Provider; each provider reads its own sub-block, so a
// single config file can carry settings for several providers and
// switch between them by changing one key.
type Config struct {
	// Provider selects the inference backend: "local" (llama.cpp,
	// in-process, requires a `-tags llama` build), "anthropic",
	// "openai", or "ollama". Empty defaults to "local".
	Provider string `mapstructure:"provider" yaml:"provider,omitempty"`

	// MaxSteps caps the agent tool-loop. Provider-agnostic. Defaults
	// to 16.
	MaxSteps int `mapstructure:"max_steps" yaml:"max_steps,omitempty"`

	// Local configures the in-process llama.cpp provider.
	Local LocalConfig `mapstructure:"local" yaml:"local,omitempty"`
	// Anthropic configures the hosted Anthropic Messages API provider.
	Anthropic RemoteConfig `mapstructure:"anthropic" yaml:"anthropic,omitempty"`
	// OpenAI configures the hosted OpenAI Chat Completions provider.
	OpenAI RemoteConfig `mapstructure:"openai" yaml:"openai,omitempty"`
	// Ollama configures a local/remote Ollama daemon provider.
	Ollama OllamaConfig `mapstructure:"ollama" yaml:"ollama,omitempty"`
}

// LocalConfig is the `llm.local:` sub-block — settings for the
// in-process llama.cpp provider.
type LocalConfig struct {
	// Model is the path to a .gguf model file. Required for the local
	// provider — empty disables it.
	Model string `mapstructure:"model" yaml:"model,omitempty"`
	// Ctx is the context window in tokens. Defaults to 4096.
	Ctx int `mapstructure:"ctx" yaml:"ctx,omitempty"`
	// GPULayers is the number of layers to offload to GPU (Metal /
	// CUDA). 999 = all, 0 = CPU-only. Defaults to 999.
	GPULayers int `mapstructure:"gpu_layers" yaml:"gpu_layers,omitempty"`
	// Template is the chat-template family: "chatml" (Qwen2.5,
	// Hermes-3) or "llama3" (Llama-3.x native). Defaults to "chatml".
	Template string `mapstructure:"template" yaml:"template,omitempty"`
}

// RemoteConfig is the sub-block shared by the HTTP API providers
// (Anthropic, OpenAI).
type RemoteConfig struct {
	// Model is the API model identifier (e.g. "claude-sonnet-4-6",
	// "gpt-4o"). Defaulted per provider by ApplyDefaults.
	Model string `mapstructure:"model" yaml:"model,omitempty"`
	// APIKeyEnv names the environment variable holding the API key.
	// Defaulted per provider by ApplyDefaults. The key itself is never
	// stored in the config file.
	APIKeyEnv string `mapstructure:"api_key_env" yaml:"api_key_env,omitempty"`
	// BaseURL overrides the API endpoint (proxies, gateways, Azure).
	// Defaulted per provider by ApplyDefaults.
	BaseURL string `mapstructure:"base_url" yaml:"base_url,omitempty"`
}

// OllamaConfig is the `llm.ollama:` sub-block.
type OllamaConfig struct {
	// Model is the Ollama model tag (e.g. "qwen2.5-coder:7b").
	// Required for the Ollama provider — empty disables it.
	Model string `mapstructure:"model" yaml:"model,omitempty"`
	// Host is the Ollama daemon base URL. Defaults to
	// "http://localhost:11434".
	Host string `mapstructure:"host" yaml:"host,omitempty"`
}

// Default endpoints / key env vars, applied by ApplyDefaults.
const (
	defaultAnthropicModel   = "claude-sonnet-4-6"
	defaultAnthropicBaseURL = "https://api.anthropic.com"
	defaultAnthropicKeyEnv  = "ANTHROPIC_API_KEY"

	defaultOpenAIModel   = "gpt-4o"
	defaultOpenAIBaseURL = "https://api.openai.com"
	defaultOpenAIKeyEnv  = "OPENAI_API_KEY"

	defaultOllamaHost = "http://localhost:11434"
)

// ProviderName returns the effective provider, applying the "local"
// default for an empty value.
func (c Config) ProviderName() string {
	if strings.TrimSpace(c.Provider) == "" {
		return "local"
	}
	return strings.ToLower(strings.TrimSpace(c.Provider))
}

// IsEnabled reports whether the config carries enough to start the
// active provider. A provider is enabled once its required fields are
// set: the local and Ollama providers need a model; the hosted
// providers need a model (defaulted) — the API key is validated at
// provider-construction time, not here.
func (c Config) IsEnabled() bool {
	switch c.ProviderName() {
	case "local":
		return strings.TrimSpace(c.Local.Model) != ""
	case "anthropic":
		return strings.TrimSpace(c.Anthropic.Model) != ""
	case "openai":
		return strings.TrimSpace(c.OpenAI.Model) != ""
	case "ollama":
		return strings.TrimSpace(c.Ollama.Model) != ""
	default:
		return false
	}
}

// MergeEnv overlays any GORTEX_LLM_* env var on top of the file
// values, then applies defaults. Env wins over file. GORTEX_LLM_MODEL
// targets the *active* provider's model so the common "swap the
// model" case needs only one variable.
func (c Config) MergeEnv() Config {
	if v := os.Getenv("GORTEX_LLM_PROVIDER"); v != "" {
		c.Provider = v
	}
	if v := os.Getenv("GORTEX_LLM_MAX_STEPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxSteps = n
		}
	}
	if v := os.Getenv("GORTEX_LLM_MODEL"); v != "" {
		switch c.ProviderName() {
		case "anthropic":
			c.Anthropic.Model = v
		case "openai":
			c.OpenAI.Model = v
		case "ollama":
			c.Ollama.Model = v
		default:
			c.Local.Model = v
		}
	}
	if v := os.Getenv("GORTEX_LLM_CTX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Local.Ctx = n
		}
	}
	if v := os.Getenv("GORTEX_LLM_GPU_LAYERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Local.GPULayers = n
		}
	}
	if v := os.Getenv("GORTEX_LLM_TEMPLATE"); v != "" {
		c.Local.Template = v
	}
	return c.ApplyDefaults()
}

// ApplyDefaults fills zero-valued fields with the canonical defaults.
// Called by MergeEnv; safe to call standalone and idempotent.
func (c Config) ApplyDefaults() Config {
	if strings.TrimSpace(c.Provider) == "" {
		c.Provider = "local"
	}
	if c.MaxSteps == 0 {
		c.MaxSteps = 16
	}

	// local
	if c.Local.Ctx == 0 {
		c.Local.Ctx = 4096
	}
	if c.Local.GPULayers == 0 {
		// 0 is indistinguishable from "unset" at the struct level; the
		// default offloads all layers. A user wanting CPU-only sets a
		// negative value? No — convention: explicit 0 in YAML still
		// reads as 0 here, so we can't honour CPU-only via this field
		// cleanly. 999 is the safe, fast default.
		c.Local.GPULayers = 999
	}
	if c.Local.Template == "" {
		c.Local.Template = "chatml"
	}

	// anthropic
	if c.Anthropic.Model == "" {
		c.Anthropic.Model = defaultAnthropicModel
	}
	if c.Anthropic.APIKeyEnv == "" {
		c.Anthropic.APIKeyEnv = defaultAnthropicKeyEnv
	}
	if c.Anthropic.BaseURL == "" {
		c.Anthropic.BaseURL = defaultAnthropicBaseURL
	}

	// openai
	if c.OpenAI.Model == "" {
		c.OpenAI.Model = defaultOpenAIModel
	}
	if c.OpenAI.APIKeyEnv == "" {
		c.OpenAI.APIKeyEnv = defaultOpenAIKeyEnv
	}
	if c.OpenAI.BaseURL == "" {
		c.OpenAI.BaseURL = defaultOpenAIBaseURL
	}

	// ollama
	if c.Ollama.Host == "" {
		c.Ollama.Host = defaultOllamaHost
	}

	return c
}

// MergedWith returns c with each zero-valued field filled from fb.
// Non-zero fields of c always win — including an explicit per-repo
// override of an inherited global value. Used to layer a repo-local
// Config (c) over a global user Config (fb). Call before ApplyDefaults
// so genuine zero values still merge.
func (c Config) MergedWith(fb Config) Config {
	if c.Provider == "" {
		c.Provider = fb.Provider
	}
	if c.MaxSteps == 0 {
		c.MaxSteps = fb.MaxSteps
	}
	c.Local = c.Local.mergedWith(fb.Local)
	c.Anthropic = c.Anthropic.mergedWith(fb.Anthropic)
	c.OpenAI = c.OpenAI.mergedWith(fb.OpenAI)
	c.Ollama = c.Ollama.mergedWith(fb.Ollama)
	return c
}

func (l LocalConfig) mergedWith(fb LocalConfig) LocalConfig {
	if l.Model == "" {
		l.Model = fb.Model
	}
	if l.Ctx == 0 {
		l.Ctx = fb.Ctx
	}
	if l.GPULayers == 0 {
		l.GPULayers = fb.GPULayers
	}
	if l.Template == "" {
		l.Template = fb.Template
	}
	return l
}

func (r RemoteConfig) mergedWith(fb RemoteConfig) RemoteConfig {
	if r.Model == "" {
		r.Model = fb.Model
	}
	if r.APIKeyEnv == "" {
		r.APIKeyEnv = fb.APIKeyEnv
	}
	if r.BaseURL == "" {
		r.BaseURL = fb.BaseURL
	}
	return r
}

func (o OllamaConfig) mergedWith(fb OllamaConfig) OllamaConfig {
	if o.Model == "" {
		o.Model = fb.Model
	}
	if o.Host == "" {
		o.Host = fb.Host
	}
	return o
}

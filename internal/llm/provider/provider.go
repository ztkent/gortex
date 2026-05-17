// Package provider is the llm.Provider factory. It is the one place
// that imports every concrete provider implementation; everything else
// (the svc layer, the agent loop, the cmd demos) depends only on the
// llm.Provider interface and calls New here.
//
// The build-tag split is contained entirely within the `local`
// subpackage: provider.New compiles in every build, and selecting
// "local" without a `-tags llama` binary surfaces as a runtime error
// from local.New, not a compile failure.
package provider

import (
	"fmt"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/provider/anthropic"
	"github.com/zzet/gortex/internal/llm/provider/bedrock"
	"github.com/zzet/gortex/internal/llm/provider/claudecli"
	"github.com/zzet/gortex/internal/llm/provider/deepseek"
	"github.com/zzet/gortex/internal/llm/provider/gemini"
	"github.com/zzet/gortex/internal/llm/provider/local"
	"github.com/zzet/gortex/internal/llm/provider/ollama"
	"github.com/zzet/gortex/internal/llm/provider/openai"
)

// New builds the llm.Provider selected by cfg.Provider. cfg should
// already have defaults applied (see llm.Config.ApplyDefaults) — the
// HTTP providers rely on the defaulted model / endpoint / key-env
// values. Returns an error when the provider is unknown or
// misconfigured (missing model, unset API key) or, for "local", when
// the binary was built without `-tags llama`; for "claudecli", when
// the `claude` binary is not on $PATH.
func New(cfg llm.Config) (llm.Provider, error) {
	switch cfg.ProviderName() {
	case "local":
		return local.New(cfg.Local)
	case "anthropic":
		return anthropic.New(cfg.Anthropic)
	case "openai":
		return openai.New(cfg.OpenAI)
	case "ollama":
		return ollama.New(cfg.Ollama)
	case "claudecli":
		return claudecli.New(cfg.ClaudeCLI)
	case "gemini":
		return gemini.New(cfg.Gemini)
	case "bedrock":
		return bedrock.New(cfg.Bedrock)
	case "deepseek":
		return deepseek.New(cfg.DeepSeek)
	default:
		return nil, fmt.Errorf("llm: unknown provider %q (want local|anthropic|openai|ollama|claudecli|gemini|bedrock|deepseek)", cfg.ProviderName())
	}
}

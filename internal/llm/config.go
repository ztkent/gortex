// Package llm — config loader for the LLM service.
//
// This file is pure Go (no build tag) so every build can compile it.
// The actual provider construction lives under internal/llm/provider/
// — the `local` provider is the only one that needs `-tags llama`.
// `claudecli` shells out to the user's `claude` binary, so it only
// needs that binary on $PATH.
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
	// "openai", "ollama", "claudecli" (subprocess against the
	// user's `claude` binary), "codex" (subprocess against the
	// user's OpenAI `codex` binary), "gemini" (Google Gemini REST
	// API), "bedrock" (AWS Bedrock Converse API, SigV4-signed) or
	// "deepseek" (DeepSeek Chat Completions, OpenAI-compatible).
	// Empty defaults to "local".
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
	// Azure configures the Azure OpenAI Service provider — the OpenAI
	// Chat Completions wire format with a deployment-in-path /
	// api-version-query / api-key-header auth model.
	Azure AzureConfig `mapstructure:"azure" yaml:"azure,omitempty"`
	// Ollama configures a local/remote Ollama daemon provider.
	Ollama OllamaConfig `mapstructure:"ollama" yaml:"ollama,omitempty"`
	// ClaudeCLI configures the Claude Code CLI subprocess provider.
	ClaudeCLI ClaudeCLIConfig `mapstructure:"claudecli" yaml:"claudecli,omitempty"`
	// Gemini configures the Google Gemini generateContent REST provider.
	Gemini RemoteConfig `mapstructure:"gemini" yaml:"gemini,omitempty"`
	// Bedrock configures the AWS Bedrock Converse API provider. It is
	// SigV4-signed; the model is picked by `model_id`
	// (e.g. "anthropic.claude-sonnet-4-20250514-v1:0").
	Bedrock BedrockConfig `mapstructure:"bedrock" yaml:"bedrock,omitempty"`
	// DeepSeek configures the hosted DeepSeek Chat Completions
	// provider (api.deepseek.com, OpenAI-compatible wire format).
	DeepSeek RemoteConfig `mapstructure:"deepseek" yaml:"deepseek,omitempty"`
	// Codex configures the OpenAI Codex CLI subprocess provider.
	Codex CodexConfig `mapstructure:"codex" yaml:"codex,omitempty"`

	// Routing configures graph-aware model routing for the `ask`
	// research agent — see RoutingConfig. Disabled by default: every
	// request runs on the active provider's configured model.
	Routing RoutingConfig `mapstructure:"routing" yaml:"routing,omitempty"`
}

// RoutingConfig is the `llm.routing:` sub-block — model routing for
// the `ask` agent. When Enabled, each agent run is classified by
// graph-derived task complexity (chain mode, multi-hop keywords,
// cross-repo scope breadth — see Classify) and dispatched to a
// cheaper or more capable model *within the active provider*: a
// trivial single-hop lookup to SimpleModel, a cross-system trace or
// refactor to ComplexModel. An empty tier model means "use the
// provider's configured model for that tier" — so routing can be set
// up to only upgrade hard tasks, or only downgrade easy ones.
type RoutingConfig struct {
	// Enabled turns model routing on. Off by default.
	Enabled bool `mapstructure:"enabled" yaml:"enabled,omitempty"`
	// SimpleModel is the model id for low-complexity agent runs (e.g.
	// "claude-haiku-4-5"). Empty falls back to the configured model.
	SimpleModel string `mapstructure:"simple_model" yaml:"simple_model,omitempty"`
	// ComplexModel is the model id for high-complexity agent runs
	// (e.g. "claude-opus-4-7"). Empty falls back to the configured
	// model.
	ComplexModel string `mapstructure:"complex_model" yaml:"complex_model,omitempty"`
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

// AzureConfig is the `llm.azure:` sub-block — settings for the Azure
// OpenAI Service provider. Azure speaks the OpenAI Chat Completions
// wire format but addresses and authenticates requests differently
// from api.openai.com: the model is selected by a deployment name
// folded into the URL path, the API contract is pinned by an
// `api-version` query parameter, and the key travels in an `api-key`
// header (not a Bearer token). A bare RemoteConfig.BaseURL override
// cannot express that, which is why Azure gets its own sub-block.
type AzureConfig struct {
	// Endpoint is the Azure OpenAI resource endpoint, e.g.
	// "https://my-resource.openai.azure.com". When empty it is read
	// from the env var named by EndpointEnv.
	Endpoint string `mapstructure:"endpoint" yaml:"endpoint,omitempty"`
	// EndpointEnv names the env var holding the endpoint when Endpoint
	// is unset. Defaults to "AZURE_OPENAI_ENDPOINT".
	EndpointEnv string `mapstructure:"endpoint_env" yaml:"endpoint_env,omitempty"`
	// Deployment is the Azure deployment name — the path segment that
	// selects the model. Required; empty disables the provider.
	Deployment string `mapstructure:"deployment" yaml:"deployment,omitempty"`
	// APIVersion pins the Azure REST API contract (e.g. "2024-10-21").
	// Defaults to a recent GA version that supports json_schema
	// structured outputs.
	APIVersion string `mapstructure:"api_version" yaml:"api_version,omitempty"`
	// Model is the logical model identifier sent in the request body.
	// Optional — Azure routes by Deployment, so this defaults to the
	// deployment name and rarely needs setting.
	Model string `mapstructure:"model" yaml:"model,omitempty"`
	// APIKeyEnv names the env var holding the Azure API key. Defaults
	// to "AZURE_OPENAI_API_KEY". The key itself is never stored in the
	// config file.
	APIKeyEnv string `mapstructure:"api_key_env" yaml:"api_key_env,omitempty"`
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

// BedrockConfig is the `llm.bedrock:` sub-block — settings for the
// AWS Bedrock Converse API provider. Authentication is by SigV4 over
// the two AWS credential env vars (key id + secret), with an optional
// session token for STS-issued credentials.
type BedrockConfig struct {
	// ModelID is the Bedrock model identifier
	// (e.g. "anthropic.claude-sonnet-4-20250514-v1:0",
	// "amazon.nova-pro-v1:0"). Required — empty disables the provider.
	ModelID string `mapstructure:"model_id" yaml:"model_id,omitempty"`
	// Region is the AWS region the model is hosted in
	// (e.g. "us-east-1"). Defaults to "us-east-1".
	Region string `mapstructure:"region" yaml:"region,omitempty"`
	// AccessKeyEnv names the env var holding the AWS access key id.
	// Defaults to "AWS_ACCESS_KEY_ID".
	AccessKeyEnv string `mapstructure:"access_key_env" yaml:"access_key_env,omitempty"`
	// SecretKeyEnv names the env var holding the AWS secret key.
	// Defaults to "AWS_SECRET_ACCESS_KEY".
	SecretKeyEnv string `mapstructure:"secret_key_env" yaml:"secret_key_env,omitempty"`
	// SessionTokenEnv names the env var holding an optional STS
	// session token. Defaults to "AWS_SESSION_TOKEN". Unset is fine.
	SessionTokenEnv string `mapstructure:"session_token_env" yaml:"session_token_env,omitempty"`
	// BaseURL overrides the endpoint — useful for VPC endpoints.
	// Defaults to "https://bedrock-runtime.<region>.amazonaws.com".
	BaseURL string `mapstructure:"base_url" yaml:"base_url,omitempty"`
}

// ClaudeCLIConfig is the `llm.claudecli:` sub-block — settings for
// the subprocess provider that shells out to the user's local
// Claude Code CLI. The binary must already be installed and signed
// in; gortex never touches credentials directly.
type ClaudeCLIConfig struct {
	// Binary is the executable name or absolute path. Empty defaults
	// to "claude" (resolved via $PATH).
	Binary string `mapstructure:"binary" yaml:"binary,omitempty"`
	// Model is the Claude model alias forwarded as `--model` (e.g.
	// "sonnet", "opus", "claude-sonnet-4-6"). Empty lets the CLI
	// pick its own default.
	Model string `mapstructure:"model" yaml:"model,omitempty"`
	// Args is a list of extra arguments appended after the provider's
	// own flags. Useful for `--allowed-tools ""` to disable tools, or
	// `--permission-mode plan` for a read-only profile.
	Args []string `mapstructure:"args" yaml:"args,omitempty"`
	// TimeoutSeconds caps one Complete call. 0 → 120s.
	TimeoutSeconds int `mapstructure:"timeout_seconds" yaml:"timeout_seconds,omitempty"`
}

// CodexConfig is the `llm.codex:` sub-block — settings for the
// subprocess provider that shells out to the user's local OpenAI
// Codex CLI. The binary must already be installed and signed in;
// gortex never touches credentials directly. Field shape mirrors
// ClaudeCLIConfig — both are CLI-subprocess providers.
type CodexConfig struct {
	// Binary is the executable name or absolute path. Empty defaults
	// to "codex" (resolved via $PATH).
	Binary string `mapstructure:"binary" yaml:"binary,omitempty"`
	// Model is the model slug forwarded as `--model` (e.g. "gpt-5-codex",
	// "o4-mini"). Empty lets the CLI pick its own default.
	Model string `mapstructure:"model" yaml:"model,omitempty"`
	// Args is a list of extra arguments inserted before the prompt
	// positional. Useful for `--sandbox workspace-write` or a `--config`
	// override when the defaults don't fit.
	Args []string `mapstructure:"args" yaml:"args,omitempty"`
	// TimeoutSeconds caps one Complete call. 0 → 180s.
	TimeoutSeconds int `mapstructure:"timeout_seconds" yaml:"timeout_seconds,omitempty"`
}

// Default endpoints / key env vars, applied by ApplyDefaults.
const (
	defaultAnthropicModel   = "claude-sonnet-4-6"
	defaultAnthropicBaseURL = "https://api.anthropic.com"
	defaultAnthropicKeyEnv  = "ANTHROPIC_API_KEY"

	defaultOpenAIModel   = "gpt-4o"
	defaultOpenAIBaseURL = "https://api.openai.com"
	defaultOpenAIKeyEnv  = "OPENAI_API_KEY"

	defaultAzureAPIVersion  = "2024-10-21"
	defaultAzureEndpointEnv = "AZURE_OPENAI_ENDPOINT"
	defaultAzureKeyEnv      = "AZURE_OPENAI_API_KEY"

	defaultOllamaHost = "http://localhost:11434"

	defaultClaudeCLIBinary = "claude"

	defaultCodexBinary = "codex"

	defaultGeminiModel   = "gemini-2.5-pro"
	defaultGeminiBaseURL = "https://generativelanguage.googleapis.com"
	defaultGeminiKeyEnv  = "GEMINI_API_KEY"

	defaultBedrockRegion          = "us-east-1"
	defaultBedrockAccessKeyEnv    = "AWS_ACCESS_KEY_ID"
	defaultBedrockSecretKeyEnv    = "AWS_SECRET_ACCESS_KEY"
	defaultBedrockSessionTokenEnv = "AWS_SESSION_TOKEN"

	defaultDeepSeekModel   = "deepseek-chat"
	defaultDeepSeekBaseURL = "https://api.deepseek.com"
	defaultDeepSeekKeyEnv  = "DEEPSEEK_API_KEY"
)

// ProviderName returns the effective provider, applying the "local"
// default for an empty value.
func (c Config) ProviderName() string {
	if strings.TrimSpace(c.Provider) == "" {
		return "local"
	}
	return strings.ToLower(strings.TrimSpace(c.Provider))
}

// ActiveModel returns the model id of the active provider — the field
// the GORTEX_LLM_MODEL env var and Config.WithModel target. For the
// local provider this is the .gguf path; for bedrock the model_id.
func (c Config) ActiveModel() string {
	switch c.ProviderName() {
	case "anthropic":
		return c.Anthropic.Model
	case "openai":
		return c.OpenAI.Model
	case "azure":
		return c.Azure.Deployment
	case "ollama":
		return c.Ollama.Model
	case "claudecli":
		return c.ClaudeCLI.Model
	case "codex":
		return c.Codex.Model
	case "gemini":
		return c.Gemini.Model
	case "bedrock":
		return c.Bedrock.ModelID
	case "deepseek":
		return c.DeepSeek.Model
	default:
		return c.Local.Model
	}
}

// WithModel returns a copy of c with the active provider's model field
// set to model. An empty model is a no-op (returns c unchanged). Used
// by model routing to derive a per-request provider config without
// disturbing any other provider's sub-block.
func (c Config) WithModel(model string) Config {
	if strings.TrimSpace(model) == "" {
		return c
	}
	switch c.ProviderName() {
	case "anthropic":
		c.Anthropic.Model = model
	case "openai":
		c.OpenAI.Model = model
	case "azure":
		c.Azure.Deployment = model
	case "ollama":
		c.Ollama.Model = model
	case "claudecli":
		c.ClaudeCLI.Model = model
	case "codex":
		c.Codex.Model = model
	case "gemini":
		c.Gemini.Model = model
	case "bedrock":
		c.Bedrock.ModelID = model
	case "deepseek":
		c.DeepSeek.Model = model
	default:
		c.Local.Model = model
	}
	return c
}

// IsEnabled reports whether the config carries enough to start the
// active provider. A provider is enabled once its required fields are
// set: the local and Ollama providers need a model; the hosted
// providers need a model (defaulted) — the API key is validated at
// provider-construction time, not here. The Claude CLI provider has
// no required field — `binary` defaults to "claude" and `model` is
// optional — so selecting it via Provider is sufficient.
func (c Config) IsEnabled() bool {
	switch c.ProviderName() {
	case "local":
		return strings.TrimSpace(c.Local.Model) != ""
	case "anthropic":
		return strings.TrimSpace(c.Anthropic.Model) != ""
	case "openai":
		return strings.TrimSpace(c.OpenAI.Model) != ""
	case "azure":
		return strings.TrimSpace(c.Azure.Deployment) != ""
	case "ollama":
		return strings.TrimSpace(c.Ollama.Model) != ""
	case "claudecli", "codex":
		return true
	case "gemini":
		return strings.TrimSpace(c.Gemini.Model) != ""
	case "bedrock":
		return strings.TrimSpace(c.Bedrock.ModelID) != ""
	case "deepseek":
		return strings.TrimSpace(c.DeepSeek.Model) != ""
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
		case "azure":
			c.Azure.Deployment = v
		case "ollama":
			c.Ollama.Model = v
		case "claudecli":
			c.ClaudeCLI.Model = v
		case "codex":
			c.Codex.Model = v
		case "gemini":
			c.Gemini.Model = v
		case "bedrock":
			c.Bedrock.ModelID = v
		case "deepseek":
			c.DeepSeek.Model = v
		default:
			c.Local.Model = v
		}
	}
	if v := os.Getenv("GORTEX_LLM_CLAUDECLI_BINARY"); v != "" {
		c.ClaudeCLI.Binary = v
	}
	if v := os.Getenv("GORTEX_LLM_CODEX_BINARY"); v != "" {
		c.Codex.Binary = v
	}
	if v := os.Getenv("GORTEX_LLM_BEDROCK_REGION"); v != "" {
		c.Bedrock.Region = v
	}
	if v := os.Getenv("GORTEX_LLM_AZURE_ENDPOINT"); v != "" {
		c.Azure.Endpoint = v
	}
	if v := os.Getenv("GORTEX_LLM_AZURE_DEPLOYMENT"); v != "" {
		c.Azure.Deployment = v
	}
	if v := os.Getenv("GORTEX_LLM_AZURE_API_VERSION"); v != "" {
		c.Azure.APIVersion = v
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

	// azure
	if c.Azure.APIVersion == "" {
		c.Azure.APIVersion = defaultAzureAPIVersion
	}
	if c.Azure.EndpointEnv == "" {
		c.Azure.EndpointEnv = defaultAzureEndpointEnv
	}
	if c.Azure.APIKeyEnv == "" {
		c.Azure.APIKeyEnv = defaultAzureKeyEnv
	}

	// ollama
	if c.Ollama.Host == "" {
		c.Ollama.Host = defaultOllamaHost
	}

	// claudecli
	if c.ClaudeCLI.Binary == "" {
		c.ClaudeCLI.Binary = defaultClaudeCLIBinary
	}

	// codex
	if c.Codex.Binary == "" {
		c.Codex.Binary = defaultCodexBinary
	}

	// gemini
	if c.Gemini.Model == "" {
		c.Gemini.Model = defaultGeminiModel
	}
	if c.Gemini.APIKeyEnv == "" {
		c.Gemini.APIKeyEnv = defaultGeminiKeyEnv
	}
	if c.Gemini.BaseURL == "" {
		c.Gemini.BaseURL = defaultGeminiBaseURL
	}

	// bedrock
	if c.Bedrock.Region == "" {
		c.Bedrock.Region = defaultBedrockRegion
	}
	if c.Bedrock.AccessKeyEnv == "" {
		c.Bedrock.AccessKeyEnv = defaultBedrockAccessKeyEnv
	}
	if c.Bedrock.SecretKeyEnv == "" {
		c.Bedrock.SecretKeyEnv = defaultBedrockSecretKeyEnv
	}
	if c.Bedrock.SessionTokenEnv == "" {
		c.Bedrock.SessionTokenEnv = defaultBedrockSessionTokenEnv
	}

	// deepseek
	if c.DeepSeek.Model == "" {
		c.DeepSeek.Model = defaultDeepSeekModel
	}
	if c.DeepSeek.APIKeyEnv == "" {
		c.DeepSeek.APIKeyEnv = defaultDeepSeekKeyEnv
	}
	if c.DeepSeek.BaseURL == "" {
		c.DeepSeek.BaseURL = defaultDeepSeekBaseURL
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
	c.Azure = c.Azure.mergedWith(fb.Azure)
	c.Ollama = c.Ollama.mergedWith(fb.Ollama)
	c.ClaudeCLI = c.ClaudeCLI.mergedWith(fb.ClaudeCLI)
	c.Gemini = c.Gemini.mergedWith(fb.Gemini)
	c.Bedrock = c.Bedrock.mergedWith(fb.Bedrock)
	c.DeepSeek = c.DeepSeek.mergedWith(fb.DeepSeek)
	c.Codex = c.Codex.mergedWith(fb.Codex)
	c.Routing = c.Routing.mergedWith(fb.Routing)
	return c
}

func (r RoutingConfig) mergedWith(fb RoutingConfig) RoutingConfig {
	if !r.Enabled {
		r.Enabled = fb.Enabled
	}
	if r.SimpleModel == "" {
		r.SimpleModel = fb.SimpleModel
	}
	if r.ComplexModel == "" {
		r.ComplexModel = fb.ComplexModel
	}
	return r
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

func (a AzureConfig) mergedWith(fb AzureConfig) AzureConfig {
	if a.Endpoint == "" {
		a.Endpoint = fb.Endpoint
	}
	if a.EndpointEnv == "" {
		a.EndpointEnv = fb.EndpointEnv
	}
	if a.Deployment == "" {
		a.Deployment = fb.Deployment
	}
	if a.APIVersion == "" {
		a.APIVersion = fb.APIVersion
	}
	if a.Model == "" {
		a.Model = fb.Model
	}
	if a.APIKeyEnv == "" {
		a.APIKeyEnv = fb.APIKeyEnv
	}
	return a
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

func (b BedrockConfig) mergedWith(fb BedrockConfig) BedrockConfig {
	if b.ModelID == "" {
		b.ModelID = fb.ModelID
	}
	if b.Region == "" {
		b.Region = fb.Region
	}
	if b.AccessKeyEnv == "" {
		b.AccessKeyEnv = fb.AccessKeyEnv
	}
	if b.SecretKeyEnv == "" {
		b.SecretKeyEnv = fb.SecretKeyEnv
	}
	if b.SessionTokenEnv == "" {
		b.SessionTokenEnv = fb.SessionTokenEnv
	}
	if b.BaseURL == "" {
		b.BaseURL = fb.BaseURL
	}
	return b
}

func (c ClaudeCLIConfig) mergedWith(fb ClaudeCLIConfig) ClaudeCLIConfig {
	if c.Binary == "" {
		c.Binary = fb.Binary
	}
	if c.Model == "" {
		c.Model = fb.Model
	}
	if len(c.Args) == 0 {
		c.Args = fb.Args
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = fb.TimeoutSeconds
	}
	return c
}

func (c CodexConfig) mergedWith(fb CodexConfig) CodexConfig {
	if c.Binary == "" {
		c.Binary = fb.Binary
	}
	if c.Model == "" {
		c.Model = fb.Model
	}
	if len(c.Args) == 0 {
		c.Args = fb.Args
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = fb.TimeoutSeconds
	}
	return c
}

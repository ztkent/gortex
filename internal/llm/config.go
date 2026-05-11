// Package llm — config loader for the in-process LLM service.
//
// This file is pure Go (no build tag) so both llama and non-llama
// builds can compile it. The actual service construction lives in
// service.go (llama) and service_stub.go (!llama).
//
// Resolution order: file values are populated by the gortex config
// loader; MergeEnv overlays any GORTEX_LLM_* env var that's set
// (env wins). Empty fields fall back to defaults applied here.
package llm

import (
	"os"
	"strconv"
	"strings"
)

// Config is the YAML-friendly LLM block. Lives alongside the rest of
// the gortex config; promoted from .gortex.yaml's `llm:` section.
type Config struct {
	// Path to a .gguf model file. Required — empty disables the
	// service entirely (no tool registered, no startup cost).
	Model string `mapstructure:"model" yaml:"model,omitempty"`

	// Context size in tokens. Defaults to 4096.
	Ctx int `mapstructure:"ctx" yaml:"ctx,omitempty"`

	// Number of layers to offload to GPU (Metal/CUDA). 999 = all.
	// 0 = CPU-only. Defaults to 999.
	GPULayers int `mapstructure:"gpu_layers" yaml:"gpu_layers,omitempty"`

	// Maximum agent loop steps before giving up. Defaults to 16.
	MaxSteps int `mapstructure:"max_steps" yaml:"max_steps,omitempty"`

	// Chat template family: "chatml" (Qwen2.5, Hermes-3) or
	// "llama3" (Llama-3.x native). Defaults to "chatml".
	Template string `mapstructure:"template" yaml:"template,omitempty"`
}

// IsEnabled reports whether the config carries enough to start a
// service. Empty Model = disabled.
func (c Config) IsEnabled() bool { return strings.TrimSpace(c.Model) != "" }

// MergeEnv overlays any GORTEX_LLM_* env var on top of the file
// values. Env wins. After merging, ApplyDefaults fills in any
// remaining zero values.
func (c Config) MergeEnv() Config {
	if v := os.Getenv("GORTEX_LLM_MODEL"); v != "" {
		c.Model = v
	}
	if v := os.Getenv("GORTEX_LLM_CTX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Ctx = n
		}
	}
	if v := os.Getenv("GORTEX_LLM_GPU_LAYERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.GPULayers = n
		}
	}
	if v := os.Getenv("GORTEX_LLM_MAX_STEPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxSteps = n
		}
	}
	if v := os.Getenv("GORTEX_LLM_TEMPLATE"); v != "" {
		c.Template = v
	}
	return c.ApplyDefaults()
}

// ApplyDefaults fills zero-valued fields with the canonical defaults.
// Called by MergeEnv; safe to call standalone.
func (c Config) ApplyDefaults() Config {
	if c.Ctx == 0 {
		c.Ctx = 4096
	}
	if c.GPULayers == 0 {
		// 0 stays 0 only if the user explicitly set it; we can't
		// distinguish "set to 0" from "not set" at the struct level.
		// Convention: 0 means CPU-only is what the user wants iff
		// they put it in the YAML or env. For default-zero we pick
		// 999 (offload all layers).
		c.GPULayers = 999
	}
	if c.MaxSteps == 0 {
		c.MaxSteps = 16
	}
	if c.Template == "" {
		c.Template = "chatml"
	}
	return c
}

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/embedding"
)

// embedderRequest carries the explicit, per-invocation embedding inputs
// a command collected from its flags and environment. resolveEmbedder
// merges these with the on-disk config to decide what provider — if
// any — to construct.
type embedderRequest struct {
	// flagChanged reports whether the `--embeddings` boolean flag was
	// explicitly set on the command line (cmd.Flags().Changed). Only
	// an explicitly-set flag overrides the config; an untouched flag
	// leaves the decision to `embedding:` in .gortex.yaml.
	flagChanged bool
	// flagEnabled is the value of the `--embeddings` flag. Meaningful
	// only when flagChanged is true.
	flagEnabled bool
	// flagURL / flagModel are the `--embeddings-url` / `--embeddings-model`
	// flag values. A non-empty URL forces the API provider regardless
	// of every other input — it is the most explicit request possible.
	flagURL   string
	flagModel string
}

// resolveEmbedder decides which embedding.Provider (if any) a command
// should install, applying a fixed precedence:
//
//  1. An explicit embedding URL — from the `--embeddings-url` flag or
//     the GORTEX_EMBEDDINGS_URL environment variable — forces the API
//     provider. Most explicit request, highest precedence.
//  2. An explicit on/off signal — the `--embeddings` flag (when set)
//     or a GORTEX_EMBEDDINGS environment variable set to a truthy /
//     falsy value — decides enablement; when enabled, the provider
//     comes from the `embedding:` config block (default: static).
//  3. Otherwise the `embedding:` config block decides. Its default is
//     semantic search ON with the zero-download static GloVe provider,
//     so a stock install gets semantic search with no flags at all.
//
// The returned string is a short human-readable description of the
// decision for logging; it is "" when no embedder was constructed.
// A non-nil error means an embedder was requested but could not be
// built (bad provider name, API provider without a URL, …) — the
// caller should surface it and fall back to text-only search.
func resolveEmbedder(req embedderRequest, cfg *config.Config) (embedding.Provider, string, error) {
	// (1) An explicit URL — flag or env — is the most specific request.
	if url := firstNonEmpty(req.flagURL, os.Getenv("GORTEX_EMBEDDINGS_URL")); url != "" {
		model := firstNonEmpty(req.flagModel, os.Getenv("GORTEX_EMBEDDINGS_MODEL"))
		return embedding.NewAPIProvider(url, model), fmt.Sprintf("api (%s)", url), nil
	}

	embCfg := config.EmbeddingConfig{}
	if cfg != nil {
		embCfg = cfg.Embedding
	}

	// (2) An explicit on/off signal overrides the config's enablement.
	// The flag wins over the env; either, when present, is honored.
	explicitEnabled, haveExplicit := explicitEmbeddingToggle(req)
	if haveExplicit {
		if !explicitEnabled {
			return nil, "", nil
		}
		return buildConfiguredEmbedder(embCfg, "enabled by flag/env")
	}

	// (3) No explicit signal — the config decides. Default is on.
	if !embCfg.EmbeddingEnabledOrDefault() {
		return nil, "", nil
	}
	return buildConfiguredEmbedder(embCfg, "enabled by config default")
}

// explicitEmbeddingToggle reports whether the caller gave an explicit
// on/off signal for embeddings, and what it was. The `--embeddings`
// flag takes precedence over the GORTEX_EMBEDDINGS environment
// variable; an unset flag and an unset/empty env yield haveExplicit
// false so the config default applies.
func explicitEmbeddingToggle(req embedderRequest) (enabled, haveExplicit bool) {
	if req.flagChanged {
		return req.flagEnabled, true
	}
	env := strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_EMBEDDINGS")))
	switch env {
	case "1", "true", "yes", "on", "y":
		return true, true
	case "0", "false", "no", "off", "n":
		return false, true
	default:
		return false, false
	}
}

// buildConfiguredEmbedder constructs the provider named by the config
// block (defaulting to the static GloVe provider) and returns it with
// a log-friendly description that records the provider and why it was
// chosen.
func buildConfiguredEmbedder(embCfg config.EmbeddingConfig, why string) (embedding.Provider, string, error) {
	provider := embCfg.EmbeddingProviderOrDefault()
	p, err := embedding.NewProviderFromConfig(embedding.ProviderConfig{
		Provider: provider,
		APIURL:   embCfg.APIURL,
		APIModel: embCfg.APIModel,
	})
	if err != nil {
		return nil, "", err
	}
	return p, fmt.Sprintf("%s — %s", provider, why), nil
}

// embeddingChunkOptions translates the chunking knobs of an
// EmbeddingConfig into the embedding package's ChunkOptions. Zero
// values pass through — the chunker substitutes its own defaults.
func embeddingChunkOptions(cfg *config.Config) embedding.ChunkOptions {
	if cfg == nil {
		return embedding.ChunkOptions{}
	}
	return embedding.ChunkOptions{
		ThresholdLines: cfg.Embedding.ChunkThresholdLines,
		WindowLines:    cfg.Embedding.ChunkWindowLines,
	}
}

// Package embedding provides pluggable embedding providers for semantic search.
//
// The default build includes the Hugot provider (pure-Go ONNX runtime via
// hugot.NewGoSession) which auto-downloads MiniLM-L6-v2 on first use — no
// external runtime, no manual model placement. The legacy StaticProvider
// (GloVe word vectors) and APIProvider (Ollama/OpenAI) are also always
// available.
//
// Opt-in build tags enable faster transformer backends for users who are
// willing to manage native dependencies:
//   - embeddings_onnx  — yalue/onnxruntime_go with libonnxruntime on PATH
//   - embeddings_gomlx — hugot with XLA/PJRT plugin (~100MB auto-download)
package embedding

import (
	"context"
	"fmt"
)

// Provider generates embedding vectors from text.
type Provider interface {
	// Embed returns the embedding vector for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)

	// EmbedBatch returns embeddings for multiple texts.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)

	// Dimensions returns the embedding vector size.
	Dimensions() int

	// Close releases resources.
	Close() error
}

// NewHugotProvider exposes the pure-Go Hugot backend (MiniLM-L6-v2)
// directly, without the NewLocalProvider fallback chain. Useful when a
// caller wants a hard error if Hugot can't start (e.g. eval harnesses
// that mustn't silently degrade to static GloVe).
func NewHugotProvider() (Provider, error) { return newHugotProvider() }

// NewHugotProviderWithVariant loads a specific embedder variant from
// any registered HuggingFace repo (MiniLM variants, code-tuned models,
// …). Pass a name returned by KnownHugotVariants (e.g. "fp32",
// "qint8_arm64", "jina_code", "bge_code"). Returns an error if the
// variant name is unknown or the download/load fails.
func NewHugotProviderWithVariant(variant string) (Provider, error) {
	v, ok := LookupHugotVariant(variant)
	if !ok {
		return nil, fmt.Errorf("unknown hugot variant %q (known: %v)", variant, KnownHugotVariants())
	}
	return newHugotProviderWithSpec(v)
}

// NewLocalProvider returns the best available local embedding provider.
// Preference order: ONNX (fastest, requires libonnxruntime) → GoMLX (XLA) →
// Hugot (pure Go, always compiled in) → Static (GloVe word vectors fallback).
func NewLocalProvider() (Provider, error) {
	// Opt-in transformer backends (compiled in via build tags), then the
	// default Hugot pure-Go ONNX runtime which auto-downloads MiniLM-L6-v2
	// to ~/.cache/gortex/models/ on first use.
	factories := []func() (Provider, error){
		newONNXProvider,
		newGoMLXProvider,
		newHugotProvider,
	}
	for _, factory := range factories {
		if p, err := factory(); err == nil {
			return p, nil
		}
	}
	// Fallback: static word vectors (always available, no network).
	return NewStaticProvider()
}

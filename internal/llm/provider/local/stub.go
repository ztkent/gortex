//go:build !llama

// Package local — stub variant for builds without `-tags llama`.
//
// The real local provider is a CGO wrapper around llama.cpp; without
// the build tag it cannot be compiled. New therefore returns a clear
// error so the provider factory can fall through (or report the
// misconfiguration) instead of failing to compile. The HTTP providers
// are unaffected — they are pure Go and available in every build.
package local

import (
	"errors"

	"github.com/zzet/gortex/internal/llm"
)

// ErrUnavailable is returned by New in a non-llama build.
var ErrUnavailable = errors.New("local: provider unavailable — gortex was built without `-tags llama`")

// New reports the local provider as unavailable in this build.
func New(_ llm.LocalConfig) (llm.Provider, error) {
	return nil, ErrUnavailable
}

//go:build !llama

// Package svc — stub variant for builds without `-tags llama`. Every
// operational method returns errServiceUnavailable so callers don't
// need conditional imports; they just check Service.Enabled().
package svc

import (
	"context"
	"errors"

	"github.com/zzet/gortex/internal/llm"
)

var errServiceUnavailable = errors.New("llm: built without -tags llama; LLM service unavailable")

// Service is the pure-Go stub. Same exported surface as the llama
// build's Service so non-llama compilation succeeds without
// conditional imports at every call site. Every operational method
// returns errServiceUnavailable; lifecycle methods are no-ops.
type Service struct{}

// NewService returns a disabled stub service. The cfg and backend
// arguments are accepted for API compatibility but ignored.
func NewService(_ llm.Config, _ llm.Backend) *Service { return &Service{} }

// Enabled reports whether the service can do real work. Always false
// in the stub build — callers should use this to gate tool
// registration / docs generation features.
func (s *Service) Enabled() bool { return false }

// Generate is a no-op in the stub; returns errServiceUnavailable.
func (s *Service) Generate(_ context.Context, _ string, _ int) (string, error) {
	return "", errServiceUnavailable
}

// RunAgent is a no-op in the stub; returns errServiceUnavailable.
func (s *Service) RunAgent(_ context.Context, _ llm.RunAgentOptions) (*llm.AgentAnswer, error) {
	return nil, errServiceUnavailable
}

// Close is a no-op in the stub.
func (s *Service) Close() error { return nil }

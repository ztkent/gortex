//go:build !llama

package mcp

// registerLLMTools is the no-op stub used when gortex is built
// without `-tags llama`. The real implementation in tools_llm.go
// registers the `ask` MCP tool and wires it to the LLM service.
//
// Method exists on *Server in both build variants so NewServer can
// call s.registerLLMTools() unconditionally.
func (s *Server) registerLLMTools() {}

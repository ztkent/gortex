package llm

// AgentAnswer is the structured result of a Service.RunAgent call.
// Lives in pure Go (no build tag) so callers compile against the same
// type regardless of whether the service was built with -tags llama.
type AgentAnswer struct {
	Answer     string           `json:"answer"`
	Steps      int              `json:"steps"`
	ElapsedMs  int64            `json:"elapsed_ms"`
	ChainMode  bool             `json:"chain_mode"`
	Scope      Scope            `json:"scope"`
	Error      string           `json:"error,omitempty"`
	Transcript []TranscriptStep `json:"transcript,omitempty"`
}

// TranscriptStep is one entry in the agent's per-call transcript.
// Kind is "call", "result", or "final".
type TranscriptStep struct {
	Kind string `json:"kind"`
	Raw  string `json:"raw"`
	Tool string `json:"tool,omitempty"`
}

// RunAgentOptions controls a single Service.RunAgent invocation.
type RunAgentOptions struct {
	// Question is the user-facing natural-language query.
	Question string
	// Scope narrows the agent's tool calls to a subset of repos /
	// projects. Empty fields = no filter.
	Scope Scope
	// Chain enables the cross-system trace toolset (contracts +
	// get_dependencies) and prompt. Off by default — simple cross-repo
	// lookups don't need it.
	Chain bool
	// IncludeTranscript adds every CALL/RESULT/FINAL step to the
	// returned AgentAnswer. Off by default to keep responses compact.
	IncludeTranscript bool
	// SystemExtras overrides the default system prompt extras
	// (P2-style for simple mode, chain-style for chain mode). Empty
	// means use the default for the mode.
	SystemExtras string
}

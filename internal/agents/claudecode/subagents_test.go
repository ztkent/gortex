package claudecode

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSubAgentToolPropagation locks in the invariant that every Gortex
// sub-agent declares an explicit, graph-only MCP tool allowlist — the
// mechanism by which Gortex propagates its tools to sub-agents and prevents a
// sub-agent from escaping to Bash/Grep/Glob.
func TestSubAgentToolPropagation(t *testing.T) {
	require.Contains(t, SubAgents, "gortex-search.md")
	require.Contains(t, SubAgents, "gortex-impact.md")

	for name, def := range SubAgents {
		t.Run(name, func(t *testing.T) {
			tools := SubAgentTools(def)
			require.NotEmpty(t, tools,
				"%s must declare a tools allowlist so the sub-agent inherits Gortex tools", name)

			seen := map[string]bool{}
			for _, tool := range tools {
				require.Truef(t, strings.HasPrefix(tool, "mcp__gortex__"),
					"%s lists non-gortex tool %q — the allowlist must be graph-only (no Bash/Grep/Glob escape)", name, tool)
				require.Falsef(t, seen[tool], "%s lists duplicate tool %q", name, tool)
				seen[tool] = true
			}
			// smart_context is the shared entry point every sub-agent should hold.
			require.Truef(t, seen["mcp__gortex__smart_context"],
				"%s should grant smart_context (the one-call working-set entry point)", name)
		})
	}
}

// TestSubAgentFrontmatterNameMatchesFile guards against a rename drifting the
// frontmatter `name:` away from the installed filename.
func TestSubAgentFrontmatterNameMatchesFile(t *testing.T) {
	for name, def := range SubAgents {
		base := strings.TrimSuffix(name, ".md")
		require.Containsf(t, def, "name: "+base,
			"%s frontmatter name must match its filename", name)
	}
}

func TestSubAgentToolsParsesEmpty(t *testing.T) {
	require.Nil(t, SubAgentTools("---\nname: x\ndescription: no tools line\n---\nbody"))
}

package claudecode

import (
	"strings"
	"testing"
)

// TestCommandPRReviewAgent_ShellsTheReviewVerb asserts the agent-review skill
// instructs a coding agent to shell the review verb in its terse audience mode.
// The whole point of the skill is to replace hand-walking the review gates with
// a single `gortex review --audience agent` call, so that exact invocation must
// appear in the generated content.
func TestCommandPRReviewAgent_ShellsTheReviewVerb(t *testing.T) {
	for _, want := range []string{
		"gortex review --audience agent",
		"--format json",
		"VERDICT:",
		"file:line",
	} {
		if !strings.Contains(commandPRReviewAgent, want) {
			t.Errorf("commandPRReviewAgent must reference %q so the agent shells the verb and parses its output", want)
		}
	}
}

// TestCommandPRReviewAgent_Registered asserts the agent-review skill is wired
// into both the slash-command registry and the global-skill registry under
// matching names, so the plugin emitter and every adapter pick it up.
func TestCommandPRReviewAgent_Registered(t *testing.T) {
	if got := SlashCommands["gortex-pr-review-agent.md"]; got != commandPRReviewAgent {
		t.Error("gortex-pr-review-agent.md must map to commandPRReviewAgent in SlashCommands")
	}
	skill, ok := GlobalSkills["gortex-pr-review-agent"]
	if !ok {
		t.Fatal("gortex-pr-review-agent must be registered in GlobalSkills")
	}
	// The skill body is the frontmatter + the command body; the command body
	// must be present verbatim so a drift edit can't silently desync them.
	if !strings.Contains(skill, commandPRReviewAgent) {
		t.Error("gortex-pr-review-agent skill body must embed commandPRReviewAgent")
	}
	if !strings.HasPrefix(skill, "---\nname: gortex-pr-review-agent\n") {
		t.Error("gortex-pr-review-agent skill must carry matching frontmatter")
	}
}

package hooks

import (
	"encoding/json"
	"io"
	"os"
)

// Run reads a single hook payload from stdin, peeks at hook_event_name, and
// dispatches to the right handler. This is the single entry point that
// `.claude/settings.local.json` should register via `gortex hook`.
//
// Any error in reading or parsing results in a silent no-op — hooks must
// never block Claude Code's normal flow.
func Run(port int) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}

	var peek struct {
		HookEventName string `json:"hook_event_name"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		return
	}

	switch peek.HookEventName {
	case "PreToolUse":
		runPreToolUse(data, port)
	case "PreCompact":
		runPreCompact(data, port)
	case "Stop":
		runPostTask(data, port)
	case "SessionStart":
		runSessionStart(data)
	}
}

package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

// SessionStartInput is the JSON Claude Code sends on SessionStart. We
// only consume the fields we use; unknown fields are ignored.
type SessionStartInput struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	// Source is "startup" | "resume" | "clear" | "compact". Currently
	// unused — every source gets the same orientation block — but kept
	// here so future logic can branch.
	Source string `json:"source"`
}

// runSessionStart handles a SessionStart hook by querying the daemon
// for status and emitting an additionalContext block. The block is
// appended to the session's system prompt and visible for every turn,
// so it's the strongest "rule restated" surface we have.
//
// Graceful degradation: if the daemon socket can't be dialled, the
// hook still emits a block — but its content tells the user that
// enforcement is disabled and how to fix it.
func runSessionStart(data []byte) {
	var input SessionStartInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "SessionStart" {
		return
	}

	ctx := buildSessionStartBriefing(input.CWD)
	if ctx == "" {
		return
	}

	output := HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: ctx,
		},
	}
	out, err := json.Marshal(output)
	if err != nil {
		return
	}
	fmt.Print(string(out))
}

// sessionStartStatusFn is the seam tests use to inject a fake daemon
// status without spinning up a real socket. Production reads the
// default (queries the daemon socket directly via Control RPC).
var sessionStartStatusFn = fetchDaemonStatus

// fetchDaemonStatus dials the daemon's control socket and asks for
// status. Returns errDaemonUnreachable when the socket is missing —
// every other error is propagated so callers can surface it.
func fetchDaemonStatus() (*daemon.StatusResponse, error) {
	client, err := daemon.Dial(daemon.Handshake{
		Mode:       daemon.ModeControl,
		ClientName: "gortex-hook-sessionstart",
	})
	if err != nil {
		if errors.Is(err, daemon.ErrDaemonUnavailable) {
			return nil, errDaemonUnreachable
		}
		return nil, err
	}
	defer func() { _ = client.Close() }()

	_ = client.Conn.SetDeadline(time.Now().Add(2 * time.Second))

	resp, err := client.Control(daemon.ControlStatus, nil)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("daemon rejected status: %s", resp.ErrorMsg)
	}

	var status daemon.StatusResponse
	if err := unmarshalResult(resp.Result, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// buildSessionStartBriefing assembles the additionalContext block. It
// always emits something (even when the daemon is down) so the agent
// learns the rule applies regardless of enforcement state.
func buildSessionStartBriefing(cwd string) string {
	var sb strings.Builder
	sb.WriteString("## Gortex Session Orientation\n\n")

	status, err := sessionStartStatusFn()
	switch {
	case errors.Is(err, errDaemonUnreachable):
		sb.WriteString("⚠️  **Gortex daemon is not running.** Code-operation enforcement is disabled for this session: Read/Grep/Glob/Bash on indexed source files will not be redirected to graph tools.\n\n")
		sb.WriteString("Start it with: `gortex daemon start --detach`\n\n")
		sb.WriteString(rulePreamble())
		return sb.String()
	case err != nil:
		// Unexpected error — surface it tersely so debugging is possible.
		fmt.Fprintf(&sb, "⚠️  Gortex daemon status query failed: %v. Continuing with rule-only enforcement.\n\n", err)
		sb.WriteString(rulePreamble())
		return sb.String()
	}

	// Happy path: daemon is reachable.
	sb.WriteString(renderDaemonReadiness(status))
	sb.WriteString(renderCwdCoverage(cwd, status))
	sb.WriteString("\n")
	sb.WriteString(rulePreamble())
	return sb.String()
}

// renderDaemonReadiness summarises the daemon's overall state in one
// short paragraph: version, uptime, ready/warmup, totals.
func renderDaemonReadiness(s *daemon.StatusResponse) string {
	var totalNodes, totalEdges, totalRepos int
	totalRepos = len(s.TrackedRepos)
	for _, r := range s.TrackedRepos {
		totalNodes += r.Nodes
		totalEdges += r.Edges
	}

	var sb strings.Builder
	if s.Ready {
		fmt.Fprintf(&sb, "✓ Gortex daemon ready (v%s, uptime %s). ", s.Version, formatDuration(s.UptimeSeconds))
	} else {
		fmt.Fprintf(&sb, "⏳ Gortex daemon warming up (v%s, %s elapsed). Enforcement is partial until ready. ",
			s.Version, formatDuration(s.WarmupSeconds))
	}
	fmt.Fprintf(&sb, "%d tracked repo(s), %d nodes, %d edges across %d workspace(s).\n\n",
		totalRepos, totalNodes, totalEdges, len(s.Workspaces))
	return sb.String()
}

// renderCwdCoverage tells the user whether the cwd is covered by a
// tracked repo, by a workspace root containing tracked repos, or
// neither. The third case is the actionable one — we tell them how
// to fix it without doing anything ourselves.
func renderCwdCoverage(cwd string, s *daemon.StatusResponse) string {
	if cwd == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}

	exact, contained := classifyCwd(abs, s.TrackedRepos)
	switch {
	case exact != nil:
		return fmt.Sprintf("**cwd `%s` is tracked** as repo `%s` (workspace: `%s`, %d nodes). Enforcement is active.\n",
			abs, exact.Name, exact.Workspace, exact.Nodes)
	case len(contained) > 0:
		// cwd is a parent of one or more tracked repos.
		names := make([]string, 0, len(contained))
		for _, r := range contained {
			names = append(names, r.Name)
		}
		sort.Strings(names)
		shown := names
		extra := 0
		if len(shown) > 8 {
			shown = shown[:8]
			extra = len(names) - 8
		}
		summary := strings.Join(shown, ", ")
		if extra > 0 {
			summary = fmt.Sprintf("%s, +%d more", summary, extra)
		}
		return fmt.Sprintf("**cwd `%s` is a workspace root** containing %d tracked repo(s): %s. Enforcement is active for files inside these repos.\n",
			abs, len(contained), summary)
	default:
		return fmt.Sprintf("⚠️  **cwd `%s` is not covered by any tracked repo.** Read/Grep/Glob/Bash will fall through to soft guidance only — graph tools won't be available for this directory.\n\nTo enable enforcement: `gortex track %s`\n",
			abs, abs)
	}
}

// classifyCwd partitions the relationship between cwd and the daemon's
// tracked repos. Returns either an exact match (cwd == repo path) or
// the list of tracked repos contained under cwd (workspace-root case).
func classifyCwd(cwd string, repos []daemon.TrackedRepoStatus) (exact *daemon.TrackedRepoStatus, contained []daemon.TrackedRepoStatus) {
	cwd = filepath.Clean(cwd)
	for i := range repos {
		repo := repos[i]
		repoPath := filepath.Clean(repo.Path)
		if repoPath == cwd {
			exact = &repos[i]
			continue
		}
		if hasPathPrefix(repoPath, cwd) {
			contained = append(contained, repo)
		}
	}
	return exact, contained
}

// hasPathPrefix reports whether path is rooted at prefix. Filepath
// equality alone is wrong (`/foo/barbaz` would match `/foo/bar`) so
// we require either equality or that the next char after prefix is
// a separator.
func hasPathPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return len(path) > len(prefix) && (path[len(prefix)] == filepath.Separator || prefix == string(filepath.Separator))
}

// rulePreamble is the short, always-present rule restatement. The
// full table lives in ~/.claude/CLAUDE.md (added by gortex install)
// — this is just enough that an agent in the very first turn knows
// to reach for graph tools first.
func rulePreamble() string {
	return "**Rule:** Use Gortex MCP tools for code operations in this repo. Prefer:\n" +
		"- `search_symbols` / `find_usages` / `get_callers` over `grep` / `Grep`\n" +
		"- `get_symbol_source` / `get_file_summary` / `get_editing_context` over `Read`\n" +
		"- `smart_context` over multiple Read/Grep calls when starting a task\n" +
		"- `edit_symbol` / `edit_file` / `rename_symbol` over `Edit` / `Write` for indexed source\n\n" +
		"Pre-tool hooks will deny attempts to Read/Grep/Glob indexed source files; the deny message names the right tool.\n"
}

// formatDuration renders a number of seconds as "1h7m" or "45s".
// Tighter than time.Duration.String() and avoids "0s" tails.
func formatDuration(secs int64) string {
	if secs <= 0 {
		return "0s"
	}
	d := time.Duration(secs) * time.Second
	hours := int(d / time.Hour)
	mins := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	switch {
	case hours > 0:
		if mins > 0 {
			return fmt.Sprintf("%dh%dm", hours, mins)
		}
		return fmt.Sprintf("%dh", hours)
	case mins > 0:
		if s > 0 {
			return fmt.Sprintf("%dm%ds", mins, s)
		}
		return fmt.Sprintf("%dm", mins)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

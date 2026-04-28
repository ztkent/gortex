package hooks

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

func withFakeStatus(t *testing.T, fn func() (*daemon.StatusResponse, error)) {
	t.Helper()
	prev := sessionStartStatusFn
	sessionStartStatusFn = fn
	t.Cleanup(func() { sessionStartStatusFn = prev })
}

func TestRunSessionStart_RejectsWrongEvent(t *testing.T) {
	data := []byte(`{"hook_event_name":"PreCompact"}`)
	out := captureStdout(t, func() { runSessionStart(data) })
	if out != "" {
		t.Errorf("expected no-op for non-SessionStart, got: %q", out)
	}
}

func TestRunSessionStart_DaemonDown(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return nil, errDaemonUnreachable
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/x","source":"startup"}`)
	out := captureStdout(t, func() { runSessionStart(data) })
	if out == "" {
		t.Fatal("expected briefing output even when daemon is down")
	}

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "daemon is not running") {
		t.Errorf("expected daemon-down notice, got:\n%s", ac)
	}
	if !strings.Contains(ac, "gortex daemon start") {
		t.Errorf("expected start command, got:\n%s", ac)
	}
	if !strings.Contains(ac, "Rule:") {
		t.Errorf("rule preamble missing, got:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonReady_CwdExactMatch(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version:       "0.15.0",
			UptimeSeconds: 3600,
			Ready:         true,
			TrackedRepos: []daemon.TrackedRepoStatus{
				{Name: "gortex", Path: "/Users/zzet/code/my/gortex/gortex", Workspace: "gortex", Nodes: 6604, Edges: 27403},
				{Name: "cloud_web", Path: "/Users/zzet/code/my/gortex/cloud_web", Workspace: "cloud_web", Nodes: 265, Edges: 276},
			},
			Workspaces: []daemon.WorkspaceSummary{
				{Slug: "gortex"}, {Slug: "cloud_web"},
			},
		}, nil
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/Users/zzet/code/my/gortex/gortex"}`)
	out := captureStdout(t, func() { runSessionStart(data) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "daemon ready") {
		t.Errorf("expected ready marker, got:\n%s", ac)
	}
	if !strings.Contains(ac, "is tracked** as repo `gortex`") {
		t.Errorf("expected exact-match cwd line, got:\n%s", ac)
	}
	if !strings.Contains(ac, "uptime 1h") {
		t.Errorf("expected formatted uptime, got:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonReady_CwdContainsRepos(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version:       "0.15.0",
			UptimeSeconds: 60,
			Ready:         true,
			TrackedRepos: []daemon.TrackedRepoStatus{
				{Name: "gortex", Path: "/Users/zzet/code/my/gortex/gortex"},
				{Name: "cloud_web", Path: "/Users/zzet/code/my/gortex/cloud_web"},
				{Name: "labrador", Path: "/Users/zzet/code/work/mpg/labrador"},
			},
		}, nil
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/Users/zzet/code/my/gortex"}`)
	out := captureStdout(t, func() { runSessionStart(data) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "is a workspace root** containing 2 tracked repo(s)") {
		t.Errorf("expected workspace-root summary, got:\n%s", ac)
	}
	if !strings.Contains(ac, "cloud_web") || !strings.Contains(ac, "gortex") {
		t.Errorf("expected sub-repo names, got:\n%s", ac)
	}
	if strings.Contains(ac, "labrador") {
		t.Errorf("unrelated repo leaked into briefing:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonReady_CwdNotTracked(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "0.15.0",
			Ready:   true,
			TrackedRepos: []daemon.TrackedRepoStatus{
				{Name: "gortex", Path: "/Users/zzet/code/my/gortex/gortex"},
			},
		}, nil
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/playground"}`)
	out := captureStdout(t, func() { runSessionStart(data) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "is not covered by any tracked repo") {
		t.Errorf("expected untracked notice, got:\n%s", ac)
	}
	if !strings.Contains(ac, "gortex track /tmp/playground") {
		t.Errorf("expected actionable track command, got:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonWarmup(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version:       "0.15.0",
			Ready:         false,
			WarmupSeconds: 30,
			TrackedRepos:  []daemon.TrackedRepoStatus{},
		}, nil
	})
	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/x"}`)
	out := captureStdout(t, func() { runSessionStart(data) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "warming up") {
		t.Errorf("expected warmup notice, got:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonError(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return nil, errors.New("synthetic transport failure")
	})
	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/x"}`)
	out := captureStdout(t, func() { runSessionStart(data) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "status query failed") {
		t.Errorf("expected error surface, got:\n%s", ac)
	}
	if !strings.Contains(ac, "Rule:") {
		t.Errorf("rule preamble must still appear on error path, got:\n%s", ac)
	}
}

func TestDispatch_RoutesSessionStart(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "0.15.0",
			Ready:   true,
		}, nil
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp"}`)
	withStdin(t, data, func() {
		out := captureStdout(t, func() { Run(0) })
		if !strings.Contains(out, "Gortex Session Orientation") {
			t.Errorf("Run did not route SessionStart:\n%s", out)
		}
	})
}

func TestHasPathPrefix(t *testing.T) {
	cases := []struct {
		path, prefix string
		want         bool
	}{
		{"/foo/bar", "/foo", true},
		{"/foo/bar", "/foo/bar", true},
		{"/foo/barbaz", "/foo/bar", false}, // not a real subpath
		{"/foo", "/foo/bar", false},
		{"/foo/bar/baz", "/foo/bar", true},
		{"/foo", "/", true},
	}
	for _, c := range cases {
		got := hasPathPrefix(c.path, c.prefix)
		if got != c.want {
			t.Errorf("hasPathPrefix(%q, %q) = %v, want %v", c.path, c.prefix, got, c.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{0, "0s"},
		{45, "45s"},
		{60, "1m"},
		{125, "2m5s"},
		{3600, "1h"},
		{3660, "1h1m"},
	}
	for _, c := range cases {
		got := formatDuration(c.secs)
		if got != c.want {
			t.Errorf("formatDuration(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}

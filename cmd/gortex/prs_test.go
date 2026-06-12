package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/forge"
)

// resetPRsSeams restores the prs.go package-var seams and flags after a test
// mutates them, so cases stay independent.
func resetPRsSeams(t *testing.T) {
	t.Helper()
	availPrev := forgeAvailable
	listPrev := forgeListPRs
	filesPrev := forgePRFiles
	toolPrev := prsDaemonTool
	basePrev := prsBase
	repoPrev := prsRepo
	fmtPrev := prsFormat
	wtPrev := prsWorktrees
	triagePrev := prsTriage
	conflictsPrev := prsConflicts
	useLLMPrev := prsUseLLM
	t.Cleanup(func() {
		forgeAvailable = availPrev
		forgeListPRs = listPrev
		forgePRFiles = filesPrev
		prsDaemonTool = toolPrev
		prsBase = basePrev
		prsRepo = repoPrev
		prsFormat = fmtPrev
		prsWorktrees = wtPrev
		prsTriage = triagePrev
		prsConflicts = conflictsPrev
		prsUseLLM = useLLMPrev
	})
	// Deterministic defaults for every case.
	prsBase = ""
	prsRepo = ""
	prsFormat = "text"
	prsWorktrees = false
	prsTriage = false
	prsConflicts = false
	prsUseLLM = false
}

// runPRsCmd invokes the prs command with args, capturing its stdout.
func runPRsCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := *prsCmd // shallow copy so SetArgs/SetOut don't leak across cases
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := runPRs(&cmd, args)
	return buf.String(), err
}

func cannedPRs() []forge.PR {
	now := time.Now().UTC()
	return []forge.PR{
		{
			Number:    7,
			Title:     "Add forge",
			Author:    "alice",
			BaseRef:   "main",
			HeadRef:   "feature",
			UpdatedAt: now.Add(-2 * 24 * time.Hour),
			CIRollup:  "SUCCESS",
		},
		{
			Number:         8,
			Title:          "Refactor resolver",
			Author:         "bob",
			BaseRef:        "main",
			HeadRef:        "refactor",
			ReviewDecision: "CHANGES_REQUESTED",
			UpdatedAt:      now.Add(-1 * 24 * time.Hour),
			CIRollup:       "FAILURE",
		},
	}
}

// TestPRsList_Table renders the dashboard table for an authed repo from
// canned PRs and asserts the rows show up with their classifications.
func TestPRsList_Table(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return true }
	forgeListPRs = func(_ context.Context, _ string, _ forge.ListOpts) ([]forge.PR, error) {
		return cannedPRs(), nil
	}
	prsBase = "main" // pin the base so DefaultBranch probing doesn't run

	out, err := runPRsCmd(t)
	if err != nil {
		t.Fatalf("runPRs: %v", err)
	}
	for _, want := range []string{
		"STATE", "CI", "REVIEW", "AUTHOR", "TITLE",
		"7", "Add forge", "alice", "SUCCESS", "READY",
		"8", "Refactor resolver", "bob", "FAILURE", "CHANGES_REQUESTED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n----\n%s\n----", want, out)
		}
	}
}

// TestPRsList_BaseFlipsMismatch asserts --base re-targets the default base so
// a PR onto a different branch classifies as BASE_MISMATCH.
func TestPRsList_BaseFlipsMismatch(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return true }
	forgeListPRs = func(_ context.Context, _ string, _ forge.ListOpts) ([]forge.PR, error) {
		return cannedPRs(), nil
	}
	prsFormat = "json"

	// Default base == "main": PR #7 (onto main) is READY, not BASE_MISMATCH.
	prsBase = "main"
	outMain, err := runPRsCmd(t)
	if err != nil {
		t.Fatalf("runPRs (base=main): %v", err)
	}
	mainRows := decodePRRows(t, outMain)
	if got := stateOf(mainRows, 7); got == "BASE_MISMATCH" {
		t.Errorf("PR #7 onto main should NOT be BASE_MISMATCH, got %q", got)
	}

	// --base develop: every PR (all onto main) now BASE_MISMATCH.
	prsBase = "develop"
	outDev, err := runPRsCmd(t)
	if err != nil {
		t.Fatalf("runPRs (base=develop): %v", err)
	}
	devRows := decodePRRows(t, outDev)
	if got := stateOf(devRows, 7); got != "BASE_MISMATCH" {
		t.Errorf("PR #7 with --base develop should be BASE_MISMATCH, got %q", got)
	}
	// And the blocker list must name base-mismatch.
	if !hasBlocker(devRows, 7, "base-mismatch") {
		t.Errorf("PR #7 blockers should include base-mismatch: %+v", devRows)
	}
}

// TestPRsList_JSONShape asserts --format json round-trips the documented
// {prs:[{number,title,author,age_days,ci,review,state,blockers}]} shape.
func TestPRsList_JSONShape(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return true }
	forgeListPRs = func(_ context.Context, _ string, _ forge.ListOpts) ([]forge.PR, error) {
		return cannedPRs(), nil
	}
	prsBase = "main"
	prsFormat = "json"

	out, err := runPRsCmd(t)
	if err != nil {
		t.Fatalf("runPRs: %v", err)
	}

	var payload struct {
		PRs []struct {
			Number   int      `json:"number"`
			Title    string   `json:"title"`
			Author   string   `json:"author"`
			AgeDays  int      `json:"age_days"`
			CI       string   `json:"ci"`
			Review   string   `json:"review"`
			State    string   `json:"state"`
			Blockers []string `json:"blockers"`
		} `json:"prs"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode json output: %v\n%s", err, out)
	}
	if len(payload.PRs) != 2 {
		t.Fatalf("got %d PRs, want 2", len(payload.PRs))
	}
	p := payload.PRs[0]
	if p.Number != 7 || p.Title != "Add forge" || p.Author != "alice" {
		t.Errorf("PR[0] = %+v", p)
	}
	if p.CI != "SUCCESS" || p.State != "READY" {
		t.Errorf("PR[0] ci/state = %q/%q, want SUCCESS/READY", p.CI, p.State)
	}
	if p.AgeDays != 2 {
		t.Errorf("PR[0] age_days = %d, want 2", p.AgeDays)
	}
	if p.Blockers == nil {
		t.Errorf("blockers must be a non-nil array even when empty")
	}
	// The CHANGES_REQUESTED PR must carry its blocker.
	if !hasBlockerStruct(payload.PRs, 8, "changes-requested") {
		t.Errorf("PR #8 must list changes-requested blocker: %+v", payload.PRs)
	}
}

// TestPRsList_NoTokenHint asserts the unavailable-forge path prints an
// actionable GH_TOKEN hint and exits 0 (no error).
func TestPRsList_NoTokenHint(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return false }
	called := false
	forgeListPRs = func(_ context.Context, _ string, _ forge.ListOpts) ([]forge.PR, error) {
		called = true
		return nil, nil
	}

	out, err := runPRsCmd(t)
	if err != nil {
		t.Fatalf("no-token path must NOT return an error, got %v", err)
	}
	if called {
		t.Error("forge list must not be called when no token is available")
	}
	if !strings.Contains(out, "GH_TOKEN") {
		t.Errorf("no-token hint must name GH_TOKEN, got: %q", out)
	}
}

// TestPRsDeepDive_PassesFilesAndRenders asserts the deep-dive fetches the PR
// file set, forwards it to the daemon get_pr_impact tool as a JSON-array
// string, and renders the changed files + blast radius from the result.
func TestPRsDeepDive_PassesFilesAndRenders(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return true }
	forgePRFiles = func(_ context.Context, _ string, num int) ([]string, error) {
		if num != 7 {
			t.Errorf("PRFiles called with num=%d, want 7", num)
		}
		return []string{"internal/forge/forge.go", "internal/forge/ghrest.go"}, nil
	}

	var gotTool string
	var gotArgs map[string]any
	prsDaemonTool = func(_ string, tool string, args map[string]any) (json.RawMessage, error) {
		gotTool = tool
		gotArgs = args
		return json.RawMessage(`{
			"number": 7,
			"risk": "HIGH",
			"score": 68.5,
			"review_priorities": [{"axis":"flow","score":72.0,"reason":"wide fan-in"}],
			"changed_files": ["internal/forge/forge.go","internal/forge/ghrest.go"],
			"changed_symbols": [{"id":"internal/forge/forge.go::ListPRs","name":"ListPRs","kind":"function","file":"internal/forge/forge.go"}],
			"communities": ["c1","c2"]
		}`), nil
	}

	out, err := runPRsCmd(t, "7")
	if err != nil {
		t.Fatalf("deep-dive: %v", err)
	}

	if gotTool != "get_pr_impact" {
		t.Errorf("daemon tool = %q, want get_pr_impact", gotTool)
	}
	if n, _ := gotArgs["number"].(int); n != 7 {
		t.Errorf("number arg = %v, want 7", gotArgs["number"])
	}
	// files must be forwarded as a JSON-array-encoded string (the daemon's
	// get_pr_impact reads `files` as a JSON string).
	filesArg, ok := gotArgs["files"].(string)
	if !ok {
		t.Fatalf("files arg type = %T, want string", gotArgs["files"])
	}
	var files []string
	if err := json.Unmarshal([]byte(filesArg), &files); err != nil {
		t.Fatalf("files arg is not a JSON array: %v (%q)", err, filesArg)
	}
	if len(files) != 2 || files[0] != "internal/forge/forge.go" {
		t.Errorf("forwarded files = %v", files)
	}

	for _, want := range []string{
		"PR #7", "HIGH", "68.5",
		"internal/forge/forge.go",
		"internal/forge/ghrest.go",
		"ListPRs",
		"flow",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deep-dive output missing %q\n----\n%s\n----", want, out)
		}
	}
}

// TestPRsDeepDive_NoTokenStillCallsDaemon asserts that with no forge token
// the deep-dive omits `files` (no PRFiles fetch) and lets the daemon
// self-serve — it must not error out before reaching the daemon.
func TestPRsDeepDive_NoTokenStillCallsDaemon(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return false }
	forgePRFiles = func(context.Context, string, int) ([]string, error) {
		t.Error("PRFiles must not be called when no token is available")
		return nil, nil
	}
	var gotArgs map[string]any
	prsDaemonTool = func(_ string, _ string, args map[string]any) (json.RawMessage, error) {
		gotArgs = args
		return json.RawMessage(`{"number":7,"risk":"LOW","score":1.0,"changed_files":[],"changed_symbols":[],"communities":[]}`), nil
	}

	if _, err := runPRsCmd(t, "7"); err != nil {
		t.Fatalf("deep-dive (no token): %v", err)
	}
	if _, ok := gotArgs["files"]; ok {
		t.Errorf("files arg must be omitted when no token is available, got %v", gotArgs["files"])
	}
}

// TestPRsDeepDive_BadNumber rejects a non-numeric / non-positive PR argument.
func TestPRsDeepDive_BadNumber(t *testing.T) {
	resetPRsSeams(t)
	if _, err := runPRsCmd(t, "notanumber"); err == nil {
		t.Error("expected an error for a non-numeric PR number")
	}
	if _, err := runPRsCmd(t, "0"); err == nil {
		t.Error("expected an error for PR number 0")
	}
}

// --- decode helpers ---

func decodePRRows(t *testing.T, jsonOut string) []prRow {
	t.Helper()
	var payload struct {
		PRs []prRow `json:"prs"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &payload); err != nil {
		t.Fatalf("decode pr rows: %v\n%s", err, jsonOut)
	}
	return payload.PRs
}

func stateOf(rows []prRow, number int) string {
	for _, r := range rows {
		if r.Number == number {
			return r.State
		}
	}
	return ""
}

func hasBlocker(rows []prRow, number int, blocker string) bool {
	for _, r := range rows {
		if r.Number != number {
			continue
		}
		for _, b := range r.Blockers {
			if b == blocker {
				return true
			}
		}
	}
	return false
}

func hasBlockerStruct[T any](rows []T, number int, blocker string) bool {
	// rows is the anonymous JSON struct slice; marshal/unmarshal into prRow
	// for a uniform check.
	raw, _ := json.Marshal(rows)
	var prRows []prRow
	_ = json.Unmarshal(raw, &prRows)
	return hasBlocker(prRows, number, blocker)
}

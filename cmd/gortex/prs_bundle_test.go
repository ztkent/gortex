package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runPRsBundleCmd invokes the bundle subcommand with args, capturing its
// stdout. It mirrors runPRsCmd (prs_test.go) but drives runPRsBundle.
func runPRsBundleCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := &cobra.Command{Use: "bundle"}
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := runPRsBundle(cmd, args)
	return buf.String(), err
}

// cannedImpact is a get_pr_impact payload (with receipt) returned by the
// stubbed daemon-tool seam.
func cannedImpact() json.RawMessage {
	return json.RawMessage(`{
		"number": 7,
		"risk": "HIGH",
		"score": 68.5,
		"review_priorities": [{"axis":"flow","score":72.0,"reason":"wide fan-in"}],
		"changed_files": ["internal/forge/forge.go","internal/forge/ghrest.go"],
		"changed_symbols": [{"id":"internal/forge/forge.go::ListPRs","name":"ListPRs","kind":"function","file":"internal/forge/forge.go"}],
		"communities": ["c1","c2"],
		"blast": {"callers": []},
		"receipt": {"receipt_version":1,"risk_tier":"HIGH","next_safe_action":"add-tests","merge_blocker":false}
	}`)
}

// cannedReviewers is a suggest_reviewers payload returned by the stub.
func cannedReviewers() json.RawMessage {
	return json.RawMessage(`{
		"reviewers": [{"reviewer":"alice","kind":"person","score":5,"reasons":["codeowner"],"matched_files":["internal/forge/forge.go"]}],
		"total": 1,
		"changed_files": 2,
		"codeowners_found": true
	}`)
}

// stubBundleDaemon wires the daemon-tool seam to return the canned impact /
// reviewers payloads, recording the tool name and args of each call.
func stubBundleDaemon(t *testing.T) (calls *[]string, argsByTool map[string]map[string]any) {
	t.Helper()
	seen := []string{}
	argsByTool = map[string]map[string]any{}
	prsDaemonTool = func(_ string, tool string, args map[string]any) (json.RawMessage, error) {
		seen = append(seen, tool)
		argsByTool[tool] = args
		switch tool {
		case "get_pr_impact":
			return cannedImpact(), nil
		case "suggest_reviewers":
			return cannedReviewers(), nil
		default:
			t.Errorf("unexpected daemon tool %q", tool)
			return json.RawMessage(`{}`), nil
		}
	}
	return &seen, argsByTool
}

// TestPRsBundle_WritesWellFormedBundle asserts `prs bundle <N>` writes a JSON
// bundle carrying the PR number, changed files, the impact slice, and the
// reviewer suggestions — joined from the stubbed daemon tools, no network.
func TestPRsBundle_WritesWellFormedBundle(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return true }
	forgePRFiles = func(_ context.Context, _ string, num int) ([]string, error) {
		if num != 7 {
			t.Errorf("PRFiles called with num=%d, want 7", num)
		}
		return []string{"internal/forge/forge.go", "internal/forge/ghrest.go"}, nil
	}
	calls, argsByTool := stubBundleDaemon(t)

	dir := t.TempDir()
	out := filepath.Join(dir, "bundle.json")
	prsBundleOut = out
	t.Cleanup(func() { prsBundleOut = "" })

	stdout, err := runPRsBundleCmd(t, "7")
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if !strings.Contains(stdout, out) || !strings.Contains(stdout, "PR #7") {
		t.Errorf("stdout should report the written path + PR number, got: %q", stdout)
	}

	// Both daemon tools must have been called, get_pr_impact with receipt:true
	// and the forwarded file set.
	if len(*calls) != 2 {
		t.Fatalf("expected 2 daemon calls (impact + reviewers), got %v", *calls)
	}
	impArgs := argsByTool["get_pr_impact"]
	if impArgs == nil {
		t.Fatal("get_pr_impact was not called")
	}
	if r, _ := impArgs["receipt"].(bool); !r {
		t.Errorf("get_pr_impact must be called with receipt:true, got %v", impArgs["receipt"])
	}
	if n, _ := impArgs["number"].(int); n != 7 {
		t.Errorf("get_pr_impact number = %v, want 7", impArgs["number"])
	}
	filesArg, ok := impArgs["files"].(string)
	if !ok {
		t.Fatalf("files arg type = %T, want JSON-string", impArgs["files"])
	}
	var fwd []string
	if err := json.Unmarshal([]byte(filesArg), &fwd); err != nil || len(fwd) != 2 {
		t.Errorf("forwarded files = %q (%v)", filesArg, err)
	}
	if revArgs := argsByTool["suggest_reviewers"]; revArgs == nil {
		t.Error("suggest_reviewers was not called")
	} else if n, _ := revArgs["number"].(int); n != 7 {
		t.Errorf("suggest_reviewers number = %v, want 7", revArgs["number"])
	}

	// Assert the bundle file's JSON shape.
	bundle := readBundle(t, out)
	if bundle.BundleVersion != bundleVersion {
		t.Errorf("bundle_version = %d, want %d", bundle.BundleVersion, bundleVersion)
	}
	if bundle.Number != 7 {
		t.Errorf("number = %d, want 7", bundle.Number)
	}
	wantFiles := []string{"internal/forge/forge.go", "internal/forge/ghrest.go"}
	if len(bundle.ChangedFiles) != 2 || bundle.ChangedFiles[0] != wantFiles[0] || bundle.ChangedFiles[1] != wantFiles[1] {
		t.Errorf("changed_files = %v, want %v (sorted)", bundle.ChangedFiles, wantFiles)
	}

	// Impact slice round-trips with its risk + receipt.
	var imp struct {
		Risk    string `json:"risk"`
		Receipt struct {
			RiskTier string `json:"risk_tier"`
		} `json:"receipt"`
	}
	if err := json.Unmarshal(bundle.Impact, &imp); err != nil {
		t.Fatalf("decode impact slice: %v", err)
	}
	if imp.Risk != "HIGH" || imp.Receipt.RiskTier != "HIGH" {
		t.Errorf("impact risk/receipt = %q/%q, want HIGH/HIGH", imp.Risk, imp.Receipt.RiskTier)
	}

	// Reviewers slice round-trips with the ranked reviewer.
	var rev struct {
		Reviewers []struct {
			Reviewer string `json:"reviewer"`
		} `json:"reviewers"`
		CodeownersFound bool `json:"codeowners_found"`
	}
	if err := json.Unmarshal(bundle.Reviewers, &rev); err != nil {
		t.Fatalf("decode reviewers slice: %v", err)
	}
	if len(rev.Reviewers) != 1 || rev.Reviewers[0].Reviewer != "alice" || !rev.CodeownersFound {
		t.Errorf("reviewers slice = %+v", rev)
	}
}

// TestPRsBundle_DefaultOutPath asserts the bundle file name defaults to
// pr-<number>-bundle.json when --out is not given.
func TestPRsBundle_DefaultOutPath(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return false } // no token: skip PRFiles
	stubBundleDaemon(t)

	dir := t.TempDir()
	prevWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWd) })

	prsBundleOut = ""
	if _, err := runPRsBundleCmd(t, "7"); err != nil {
		t.Fatalf("bundle: %v", err)
	}

	want := filepath.Join(dir, "pr-7-bundle.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("default bundle path %q not written: %v", want, err)
	}
	// With no token, the impact's changed_files seed the top-level list.
	bundle := readBundle(t, want)
	if len(bundle.ChangedFiles) != 2 {
		t.Errorf("changed_files (from impact) = %v, want 2", bundle.ChangedFiles)
	}
}

// TestPRsBundle_DeterministicForUnchangedPR asserts two runs over an unchanged
// PR produce byte-identical bundle files (suitable for CI artifact diffing).
func TestPRsBundle_DeterministicForUnchangedPR(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return true }
	forgePRFiles = func(_ context.Context, _ string, _ int) ([]string, error) {
		// Deliberately unsorted to prove the writer sorts deterministically.
		return []string{"internal/forge/ghrest.go", "internal/forge/forge.go"}, nil
	}
	stubBundleDaemon(t)

	dir := t.TempDir()
	out1 := filepath.Join(dir, "a.json")
	out2 := filepath.Join(dir, "b.json")

	prsBundleOut = out1
	t.Cleanup(func() { prsBundleOut = "" })
	if _, err := runPRsBundleCmd(t, "7"); err != nil {
		t.Fatalf("bundle 1: %v", err)
	}
	prsBundleOut = out2
	if _, err := runPRsBundleCmd(t, "7"); err != nil {
		t.Fatalf("bundle 2: %v", err)
	}

	a, _ := os.ReadFile(out1)
	b, _ := os.ReadFile(out2)
	if string(a) != string(b) {
		t.Errorf("bundle output is not deterministic:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
}

// TestPRsBundle_BadNumber rejects a non-numeric / non-positive PR argument.
func TestPRsBundle_BadNumber(t *testing.T) {
	resetPRsSeams(t)
	// Guard: the daemon seam must never be reached for a bad number.
	prsDaemonTool = func(_ string, tool string, _ map[string]any) (json.RawMessage, error) {
		t.Errorf("daemon tool %q must not be called for a bad PR number", tool)
		return nil, nil
	}
	for _, bad := range []string{"notanumber", "0", "-3"} {
		if _, err := runPRsBundleCmd(t, bad); err == nil {
			t.Errorf("expected an error for PR number %q", bad)
		}
	}
}

// TestPRsBundle_ReviewersOptional asserts a suggest_reviewers failure does not
// sink the bundle: the impact slice is still written and reviewers is omitted.
func TestPRsBundle_ReviewersOptional(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return false }
	prsDaemonTool = func(_ string, tool string, _ map[string]any) (json.RawMessage, error) {
		if tool == "suggest_reviewers" {
			return nil, errReviewerUnavailable
		}
		return cannedImpact(), nil
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "bundle.json")
	prsBundleOut = out
	t.Cleanup(func() { prsBundleOut = "" })

	if _, err := runPRsBundleCmd(t, "7"); err != nil {
		t.Fatalf("bundle: %v", err)
	}
	bundle := readBundle(t, out)
	if len(bundle.Reviewers) != 0 {
		t.Errorf("reviewers must be omitted when suggest_reviewers fails, got %s", bundle.Reviewers)
	}
	if len(bundle.Impact) == 0 {
		t.Error("impact slice must still be written when reviewers fail")
	}
}

// TestWriteReviewerBundle_RejectsEmptyImpact asserts the writer refuses to
// produce a bundle without an impact payload.
func TestWriteReviewerBundle_RejectsEmptyImpact(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "bundle.json")
	if err := writeReviewerBundle(out, 7, []string{"a.go"}, nil, nil); err == nil {
		t.Error("expected an error for an empty impact payload")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("no file should be written when impact is empty")
	}
}

// readBundle decodes a written bundle file into a reviewerBundle.
func readBundle(t *testing.T, path string) reviewerBundle {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bundle %q: %v", path, err)
	}
	var b reviewerBundle
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatalf("decode bundle %q: %v\n%s", path, err, data)
	}
	return b
}

var errReviewerUnavailable = errBundle("reviewers unavailable")

type errBundle string

func (e errBundle) Error() string { return string(e) }

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestPRsTriage_Table renders the AI-ranked review queue from a canned
// triage_prs payload and asserts the rows show up highest-risk first.
func TestPRsTriage_Table(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return true }
	prsTriage = true

	var gotTool string
	var gotArgs map[string]any
	prsDaemonTool = func(_ string, tool string, args map[string]any) (json.RawMessage, error) {
		gotTool = tool
		gotArgs = args
		return json.RawMessage(`{
			"ranked": [
				{"number": 8, "title": "Refactor resolver", "author": "bob", "risk": "HIGH", "score": 68.5},
				{"number": 7, "title": "Add forge", "author": "alice", "risk": "LOW", "score": 3.0}
			],
			"total": 2,
			"llm_used": false
		}`), nil
	}

	out, err := runPRsCmd(t)
	if err != nil {
		t.Fatalf("triage: %v", err)
	}
	if gotTool != "triage_prs" {
		t.Errorf("daemon tool = %q, want triage_prs", gotTool)
	}
	if _, ok := gotArgs["use_llm"]; ok {
		t.Errorf("use_llm must be omitted without --use-llm, got %v", gotArgs["use_llm"])
	}

	for _, want := range []string{
		"Review queue", "RANK", "RISK", "SCORE", "TITLE",
		"Refactor resolver", "HIGH", "68.5",
		"Add forge", "LOW",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("triage table missing %q\n----\n%s\n----", want, out)
		}
	}

	// Highest-risk PR (#8) must be rendered before the low-risk PR (#7).
	if strings.Index(out, "Refactor resolver") > strings.Index(out, "Add forge") {
		t.Errorf("ranked order not preserved (HIGH should precede LOW):\n%s", out)
	}
}

// TestPRsTriage_UseLLM asserts --use-llm passes use_llm:true to the daemon and
// the per-PR rationale is rendered when the queue is LLM-reranked.
func TestPRsTriage_UseLLM(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return true }
	prsTriage = true
	prsUseLLM = true

	var gotArgs map[string]any
	prsDaemonTool = func(_ string, _ string, args map[string]any) (json.RawMessage, error) {
		gotArgs = args
		return json.RawMessage(`{
			"ranked": [
				{"number": 8, "title": "Refactor resolver", "author": "bob", "risk": "HIGH", "score": 68.5, "rationale": "touches the hot resolver path"}
			],
			"total": 1,
			"llm_used": true
		}`), nil
	}

	out, err := runPRsCmd(t)
	if err != nil {
		t.Fatalf("triage --use-llm: %v", err)
	}
	if v, _ := gotArgs["use_llm"].(bool); !v {
		t.Errorf("use_llm arg = %v, want true", gotArgs["use_llm"])
	}
	for _, want := range []string{"LLM-reranked", "touches the hot resolver path"} {
		if !strings.Contains(out, want) {
			t.Errorf("triage --use-llm output missing %q\n----\n%s\n----", want, out)
		}
	}
}

// TestPRsTriage_JSON asserts --triage --format json round-trips the raw
// triage_prs payload unchanged.
func TestPRsTriage_JSON(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return true }
	prsTriage = true
	prsFormat = "json"

	prsDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"ranked":[{"number":8,"title":"Refactor resolver","author":"bob","risk":"HIGH","score":68.5}],"total":1,"llm_used":false}`), nil
	}

	out, err := runPRsCmd(t)
	if err != nil {
		t.Fatalf("triage json: %v", err)
	}
	var payload struct {
		Ranked []struct {
			Number int     `json:"number"`
			Risk   string  `json:"risk"`
			Score  float64 `json:"score"`
		} `json:"ranked"`
		Total   int  `json:"total"`
		LLMUsed bool `json:"llm_used"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode triage json: %v\n%s", err, out)
	}
	if len(payload.Ranked) != 1 || payload.Ranked[0].Number != 8 || payload.Ranked[0].Risk != "HIGH" {
		t.Errorf("triage json payload = %+v", payload)
	}
	if payload.Total != 1 {
		t.Errorf("triage json total = %d, want 1", payload.Total)
	}
}

// TestPRsConflicts_Table renders the merge-order conflict clusters from a
// canned conflicts_prs payload.
func TestPRsConflicts_Table(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return true }
	prsConflicts = true

	var gotTool string
	prsDaemonTool = func(_ string, tool string, _ map[string]any) (json.RawMessage, error) {
		gotTool = tool
		return json.RawMessage(`{
			"conflicts": [
				{"community": "c1", "size": 42, "prs": [7, 8, 9], "suggested_order": [7, 9, 8], "risk": 3.81}
			],
			"total": 1
		}`), nil
	}

	out, err := runPRsCmd(t)
	if err != nil {
		t.Fatalf("conflicts: %v", err)
	}
	if gotTool != "conflicts_prs" {
		t.Errorf("daemon tool = %q, want conflicts_prs", gotTool)
	}
	for _, want := range []string{
		"Merge-order conflicts", "COMMUNITY", "SIZE", "RISK", "MERGE ORDER",
		"c1", "42", "3.81",
		"#7, #8, #9", // colliding PRs (ascending)
		"#7, #9, #8", // suggested merge order
	} {
		if !strings.Contains(out, want) {
			t.Errorf("conflicts table missing %q\n----\n%s\n----", want, out)
		}
	}
}

// TestPRsConflicts_JSON asserts --conflicts --format json round-trips the raw
// conflicts_prs payload unchanged.
func TestPRsConflicts_JSON(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return true }
	prsConflicts = true
	prsFormat = "json"

	prsDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"conflicts":[{"community":"c1","size":42,"prs":[7,8],"suggested_order":[7,8],"risk":2.8}],"total":1}`), nil
	}

	out, err := runPRsCmd(t)
	if err != nil {
		t.Fatalf("conflicts json: %v", err)
	}
	var payload struct {
		Conflicts []struct {
			Community      string  `json:"community"`
			Size           int     `json:"size"`
			PRs            []int   `json:"prs"`
			SuggestedOrder []int   `json:"suggested_order"`
			Risk           float64 `json:"risk"`
		} `json:"conflicts"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode conflicts json: %v\n%s", err, out)
	}
	if len(payload.Conflicts) != 1 || payload.Conflicts[0].Community != "c1" || payload.Conflicts[0].Size != 42 {
		t.Errorf("conflicts json payload = %+v", payload)
	}
}

// TestPRsTriageConflicts_BothRunBothSections asserts that giving both flags
// runs triage first, then conflicts, in a single invocation.
func TestPRsTriageConflicts_BothRunBothSections(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return true }
	prsTriage = true
	prsConflicts = true

	var calls []string
	prsDaemonTool = func(_ string, tool string, _ map[string]any) (json.RawMessage, error) {
		calls = append(calls, tool)
		switch tool {
		case "triage_prs":
			return json.RawMessage(`{"ranked":[{"number":8,"title":"Refactor resolver","author":"bob","risk":"HIGH","score":68.5}],"total":1,"llm_used":false}`), nil
		case "conflicts_prs":
			return json.RawMessage(`{"conflicts":[{"community":"c1","size":42,"prs":[7,8],"suggested_order":[7,8],"risk":2.8}],"total":1}`), nil
		}
		return json.RawMessage(`{}`), nil
	}

	out, err := runPRsCmd(t)
	if err != nil {
		t.Fatalf("triage+conflicts: %v", err)
	}
	if len(calls) != 2 || calls[0] != "triage_prs" || calls[1] != "conflicts_prs" {
		t.Errorf("tool call order = %v, want [triage_prs conflicts_prs]", calls)
	}
	if !strings.Contains(out, "Review queue") || !strings.Contains(out, "Merge-order conflicts") {
		t.Errorf("both sections must render:\n%s", out)
	}
	// Triage section must precede the conflicts section.
	if strings.Index(out, "Review queue") > strings.Index(out, "Merge-order conflicts") {
		t.Errorf("triage must render before conflicts:\n%s", out)
	}
}

// TestPRsTriage_NoTokenHint asserts the unavailable-forge path prints an
// actionable GH_TOKEN hint, exits 0, and never calls the daemon.
func TestPRsTriage_NoTokenHint(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return false }
	prsTriage = true
	called := false
	prsDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		called = true
		return json.RawMessage(`{}`), nil
	}

	out, err := runPRsCmd(t)
	if err != nil {
		t.Fatalf("no-token triage path must NOT return an error, got %v", err)
	}
	if called {
		t.Error("daemon tool must not be called when no token is available")
	}
	if !strings.Contains(out, "GH_TOKEN") {
		t.Errorf("no-token hint must name GH_TOKEN, got: %q", out)
	}
}

// TestPRsTriage_Degradation asserts a forge-degradation envelope from the
// daemon tool is rendered as a hint line, not a crash.
func TestPRsTriage_Degradation(t *testing.T) {
	resetPRsSeams(t)
	forgeAvailable = func(context.Context) bool { return true }
	prsTriage = true
	prsDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"error":"forge unavailable","hint":"set GH_TOKEN (or GITHUB_TOKEN) in the daemon environment"}`), nil
	}

	out, err := runPRsCmd(t)
	if err != nil {
		t.Fatalf("triage degradation: %v", err)
	}
	if !strings.Contains(out, "forge unavailable") || !strings.Contains(out, "GH_TOKEN") {
		t.Errorf("triage degradation must surface the error+hint, got: %q", out)
	}
}

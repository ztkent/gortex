package svc

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/conversationlog"
)

// fakeGenProvider answers a freeform Generate with a fixed string and
// (optionally) a usage block, so the sink can be exercised for both the
// real-usage and estimated paths.
type fakeGenProvider struct{ usage llm.TokenUsage }

func (p *fakeGenProvider) Name() string { return "fakegen" }
func (p *fakeGenProvider) Close() error { return nil }
func (p *fakeGenProvider) Complete(_ context.Context, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
	return llm.CompletionResponse{Text: "the answer", Usage: p.usage}, nil
}

func TestService_ConversationLog_Generate(t *testing.T) {
	dir := t.TempDir()
	s := newFakeService(&fakeGenProvider{usage: llm.TokenUsage{InputTokens: 12, OutputTokens: 3}})
	s.SetConversationDir(dir)
	t.Cleanup(func() { _ = s.Close() })

	ctx := conversationlog.WithMeta(context.Background(), conversationlog.Meta{
		Session: "gen-sess", Repo: "gortex", File: "pkg/x.go", Phase: "main",
	})
	out, err := s.Generate(ctx, "hello", 64)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out != "the answer" {
		t.Fatalf("Generate returned %q", out)
	}

	recs, err := conversationlog.NewReader(dir).LoadSession("gen-sess")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.File != "pkg/x.go" || r.Phase != "main" || r.Repo != "gortex" {
		t.Fatalf("labels not recorded: %+v", r)
	}
	if r.Response != "the answer" {
		t.Fatalf("response not recorded: %q", r.Response)
	}
	if r.InputTokens != 12 || r.OutputTokens != 3 {
		t.Fatalf("real usage not recorded: in=%d out=%d", r.InputTokens, r.OutputTokens)
	}
	if r.Estimated {
		t.Fatal("real usage must not be flagged estimated")
	}
}

func TestService_ConversationLog_RunAgent_OneRecordPerTurn(t *testing.T) {
	dir := t.TempDir()
	// fakeAgentProvider returns no usage → estimated counts.
	s := newFakeService(&fakeAgentProvider{})
	s.SetConversationDir(dir)
	t.Cleanup(func() { _ = s.Close() })

	ctx := conversationlog.WithMeta(context.Background(), conversationlog.Meta{Session: "agent-sess"})
	ans, err := s.RunAgent(ctx, llm.RunAgentOptions{Question: "find Foo"})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if ans.Answer == "" {
		t.Fatal("agent produced no answer")
	}

	recs, err := conversationlog.NewReader(dir).LoadSession("agent-sess")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want exactly 1 per turn", len(recs))
	}
	if !recs[0].Estimated {
		t.Fatal("a provider without usage must record estimated token counts")
	}
	if recs[0].Response != ans.Answer {
		t.Fatalf("recorded response %q != answer %q", recs[0].Response, ans.Answer)
	}
}

func TestService_ConversationLog_OptInOff_NothingRecorded(t *testing.T) {
	// Default service has no conversation dir → nothing is recorded.
	dir := t.TempDir()
	s := newFakeService(&fakeGenProvider{})
	t.Cleanup(func() { _ = s.Close() })
	if s.ConversationDir() != "" {
		t.Fatal("conversation dir should be off by default")
	}

	_, err := s.Generate(context.Background(), "hello", 64)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	sessions, err := conversationlog.NewReader(dir).ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("opt-in OFF recorded %d sessions, want 0", len(sessions))
	}
}

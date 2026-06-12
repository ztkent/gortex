package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

// stubSummarizer is a controllable llm.Provider used as a rolling-summary
// summarizer. It records how many times Complete was called, returns a fixed
// short summary with a fixed usage, and can optionally block until released
// (to exercise the async drop-after-cancel path).
type stubSummarizer struct {
	calls   int32
	summary string
	usage   llm.TokenUsage

	block   chan struct{} // when non-nil, Complete waits on it (or ctx) before returning
	started chan struct{} // closed-once signal that Complete has begun (optional)
	once    sync.Once
}

func (s *stubSummarizer) Name() string { return "stub-summarizer" }
func (s *stubSummarizer) Close() error { return nil }

func (s *stubSummarizer) Complete(ctx context.Context, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
	atomic.AddInt32(&s.calls, 1)
	if s.started != nil {
		s.once.Do(func() { close(s.started) })
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return llm.CompletionResponse{Usage: s.usage}, ctx.Err()
		}
	}
	return llm.CompletionResponse{Text: s.summary, Usage: s.usage}, nil
}

func (s *stubSummarizer) callCount() int { return int(atomic.LoadInt32(&s.calls)) }

// buildConv assembles a conversation: pinned system + first user task,
// followed by `rounds` assistant/tool round pairs whose content is `filler`.
func buildConv(rounds int, filler string) []llm.Message {
	conv := []llm.Message{
		{Role: llm.RoleSystem, Content: "SYSTEM PROMPT"},
		{Role: llm.RoleUser, Content: "the original task"},
	}
	for i := 0; i < rounds; i++ {
		conv = append(conv,
			llm.Message{Role: llm.RoleAssistant, Content: `{"tool":"search","args":{}} ` + filler},
			llm.Message{Role: llm.RoleTool, Content: "tool result " + filler, ToolName: "search"},
		)
	}
	return conv
}

func TestPartitionZones_SplitsAndPins(t *testing.T) {
	// 10 rounds, ActiveRounds=4 → last 4 rounds (8 msgs) active, the
	// remaining 6 rounds (12 msgs) compress, nothing frozen yet.
	conv := buildConv(10, "x")
	frozen, compress, active := partitionZones(conv, 4)

	if len(frozen) != 0 {
		t.Errorf("frozen = %d msgs, want 0 (no prior summary)", len(frozen))
	}
	if got := countRounds(compress); got != 6 {
		t.Errorf("compress rounds = %d, want 6", got)
	}
	if got := countRounds(active); got != 4 {
		t.Errorf("active rounds = %d, want 4", got)
	}
	// The pinned system + first user task are not in any zone.
	for _, m := range append(append(append([]llm.Message{}, frozen...), compress...), active...) {
		if m.Role == llm.RoleSystem {
			t.Errorf("system prompt leaked into a zone")
		}
		if m.Role == llm.RoleUser && m.Content == "the original task" {
			t.Errorf("first user task leaked into a zone")
		}
	}
	// active holds the most recent rounds, verbatim.
	if active[len(active)-1].Content != conv[len(conv)-1].Content {
		t.Errorf("active tail not verbatim: %q vs %q", active[len(active)-1].Content, conv[len(conv)-1].Content)
	}
}

func TestPartitionZones_FrozenSummaryStaysFrozen(t *testing.T) {
	conv := []llm.Message{
		{Role: llm.RoleSystem, Content: "SYSTEM"},
		{Role: llm.RoleUser, Content: "task"},
		{Role: llm.RoleAssistant, Content: summarySentinel + "earlier steps"},
	}
	// then 6 verbatim rounds
	conv = append(conv, buildConv(6, "y")[2:]...)

	frozen, compress, active := partitionZones(conv, 4)
	if len(frozen) != 1 || !isFrozenSummary(frozen[0]) {
		t.Fatalf("frozen = %v, want exactly the one summary note", frozen)
	}
	if countRounds(active) != 4 {
		t.Errorf("active rounds = %d, want 4", countRounds(active))
	}
	if countRounds(compress) != 2 {
		t.Errorf("compress rounds = %d, want 2", countRounds(compress))
	}
}

func TestPartitionZones_ShortConversation(t *testing.T) {
	// Only the pinned messages, no rounds.
	conv := buildConv(0, "")
	frozen, compress, active := partitionZones(conv, 4)
	if frozen != nil || compress != nil || active != nil {
		t.Errorf("short conv produced zones: f=%v c=%v a=%v", frozen, compress, active)
	}
	// Fewer rounds than ActiveRounds → everything is active, nothing compressed.
	conv = buildConv(2, "z")
	_, compress, active = partitionZones(conv, 4)
	if compress != nil {
		t.Errorf("compress = %v, want nil when rounds < ActiveRounds", compress)
	}
	if countRounds(active) != 2 {
		t.Errorf("active rounds = %d, want 2", countRounds(active))
	}
}

func TestMaybeCompact_SyncReducesBelowLowWater(t *testing.T) {
	// A big conversation: 12 rounds of long filler so it crosses the trigger.
	filler := strings.Repeat("lorem ipsum dolor sit amet ", 40)
	conv := buildConv(12, filler)

	c := NewRollingCompactor(&stubSummarizer{
		summary: "compact summary of earlier work",
		usage:   llm.TokenUsage{InputTokens: 500, OutputTokens: 20},
	}, 4, 3000, 100)
	c.sync = true

	pre := estimateConvTokens(conv, llm.TokenUsage{})
	if pre < c.CompressTriggerTokens {
		t.Fatalf("setup: pre=%d should exceed trigger=%d", pre, c.CompressTriggerTokens)
	}

	var usage llm.TokenUsage
	var mu sync.Mutex
	out, did := c.maybeCompact(context.Background(), conv, pre, &usage, &mu)
	if !did {
		t.Fatal("maybeCompact did not compact a conversation over the high-water mark")
	}
	post := estimateConvTokens(out, llm.TokenUsage{})
	if post >= pre {
		t.Errorf("post-compaction size %d not below pre %d", post, pre)
	}

	// The pinned system + first user task survive; exactly one frozen summary added.
	if out[0].Role != llm.RoleSystem || out[1].Content != "the original task" {
		t.Errorf("pinned messages not preserved: %+v", out[:2])
	}
	summaries := 0
	for _, m := range out {
		if isFrozenSummary(m) {
			summaries++
		}
	}
	if summaries != 1 {
		t.Errorf("frozen summaries = %d, want 1", summaries)
	}
	// The last ActiveRounds rounds are byte-identical to the original tail.
	for i := 1; i <= c.ActiveRounds*2; i++ {
		if out[len(out)-i].Content != conv[len(conv)-i].Content {
			t.Errorf("active tail msg -%d changed: %q vs %q", i, out[len(out)-i].Content, conv[len(conv)-i].Content)
		}
	}

	// The summarizer's usage was attributed.
	if usage.InputTokens != 500 || usage.OutputTokens != 20 {
		t.Errorf("summarizer usage not attributed: got %+v", usage)
	}
}

func TestMaybeCompact_BelowThresholdUntouched(t *testing.T) {
	conv := buildConv(3, "short")
	stub := &stubSummarizer{summary: "x"}
	c := NewRollingCompactor(stub, 4, 100000, 100)

	var usage llm.TokenUsage
	var mu sync.Mutex
	size := estimateConvTokens(conv, llm.TokenUsage{})
	out, did := c.maybeCompact(context.Background(), conv, size, &usage, &mu)
	if did {
		t.Error("compacted a conversation under the trigger")
	}
	if len(out) != len(conv) {
		t.Errorf("conversation length changed: %d vs %d", len(out), len(conv))
	}
	if stub.callCount() != 0 {
		t.Errorf("summarizer called %d times for a short conversation, want 0", stub.callCount())
	}
}

func TestMaybeCompact_NilSummarizerIsNoOp(t *testing.T) {
	conv := buildConv(20, strings.Repeat("big ", 100))
	c := NewRollingCompactor(nil, 4, 10, 5) // nil summarizer disables compaction
	var usage llm.TokenUsage
	var mu sync.Mutex
	out, did := c.maybeCompact(context.Background(), conv, 1_000_000, &usage, &mu)
	if did {
		t.Error("nil-summarizer compactor compacted")
	}
	if len(out) != len(conv) {
		t.Errorf("conversation mutated by a disabled compactor")
	}
}

func TestMaybeCompact_LateSummaryDroppedOnCancel(t *testing.T) {
	filler := strings.Repeat("lorem ipsum dolor ", 40)
	conv := buildConv(12, filler)

	block := make(chan struct{})
	started := make(chan struct{})
	stub := &stubSummarizer{
		summary: "should be dropped",
		usage:   llm.TokenUsage{InputTokens: 99, OutputTokens: 3},
		block:   block,
		started: started,
	}
	c := NewRollingCompactor(stub, 4, 3000, 100) // async (sync=false)

	ctx, cancel := context.WithCancel(context.Background())
	var usage llm.TokenUsage
	var mu sync.Mutex

	pre := estimateConvTokens(conv, llm.TokenUsage{})
	// First call spawns the background summarizer and returns unchanged.
	out, did := c.maybeCompact(ctx, conv, pre, &usage, &mu)
	if did {
		t.Fatal("async first call should not compact synchronously")
	}
	if len(out) != len(conv) {
		t.Fatal("async first call mutated the conversation")
	}

	<-started    // the summarizer goroutine is running
	cancel()     // cancel runCtx — mirrors Run's defer cancel()
	close(block) // let the summarizer finish; it must observe the cancelled ctx
	c.wait()     // drain the goroutine (what Run does)

	// A late summary must be dropped: nothing is left ready to splice, and
	// the conversation is unchanged. (Inspect the compactor state directly so
	// this assertion does not spawn a fresh concurrent summarizer.)
	c.pendingMu.Lock()
	ready := c.ready
	c.pendingMu.Unlock()
	if ready {
		t.Error("a summary produced after ctx-cancel was left ready to splice (should be dropped)")
	}

	// applyReadySummary on a drained compactor returns the conversation
	// unchanged, with no panic.
	out2, did2 := c.applyReadySummary(conv)
	if did2 {
		t.Error("a dropped summary was spliced")
	}
	if len(out2) != len(conv) {
		t.Error("conversation changed despite a dropped summary")
	}

	// Even a dropped summary's tokens are attributed — no unaccounted spend.
	// Read under mu; the goroutine has been drained by c.wait().
	mu.Lock()
	inTok, outTok := usage.InputTokens, usage.OutputTokens
	mu.Unlock()
	if inTok != 99 || outTok != 3 {
		t.Errorf("dropped-summary usage not attributed: got in=%d out=%d", inTok, outTok)
	}
}

func TestMaybeCompact_AsyncSummarySplicedNextTurn(t *testing.T) {
	filler := strings.Repeat("lorem ipsum dolor ", 40)
	conv := buildConv(12, filler)

	stub := &stubSummarizer{
		summary: "async summary text",
		usage:   llm.TokenUsage{InputTokens: 10, OutputTokens: 2},
	}
	c := NewRollingCompactor(stub, 4, 3000, 100) // async

	var usage llm.TokenUsage
	var mu sync.Mutex
	pre := estimateConvTokens(conv, llm.TokenUsage{})

	_, did := c.maybeCompact(context.Background(), conv, pre, &usage, &mu)
	if did {
		t.Fatal("first async call should not compact synchronously")
	}
	c.wait() // let the (non-blocking) summarizer finish

	out, did2 := c.maybeCompact(context.Background(), conv, pre, &usage, &mu)
	if !did2 {
		t.Fatal("a ready async summary was not spliced on the next turn")
	}
	found := false
	for _, m := range out {
		if isFrozenSummary(m) && strings.Contains(m.Content, "async summary text") {
			found = true
		}
	}
	if !found {
		t.Error("spliced conversation missing the async summary note")
	}
	if usage.InputTokens != 10 {
		t.Errorf("async summarizer usage not attributed: %+v", usage)
	}
}

func TestEstimateConvTokens_PrefersLastUsage(t *testing.T) {
	conv := buildConv(2, "some content here")
	// With a nonzero lastUsage prompt count, it wins.
	if got := estimateConvTokens(conv, llm.TokenUsage{InputTokens: 4242}); got != 4242 {
		t.Errorf("estimateConvTokens = %d, want 4242 (lastUsage preferred)", got)
	}
	// With zero usage it falls back to counting message text (nonzero).
	if got := estimateConvTokens(conv, llm.TokenUsage{}); got <= 0 {
		t.Errorf("estimateConvTokens fallback = %d, want > 0", got)
	}
}

// TestRun_LongLoopBounded drives a real Agent.Run with a tool provider that
// never finalizes, so the loop runs to maxSteps, and asserts that a compactor
// keeps the conversation bounded while a short run is left untouched.
func TestRun_CompactionWiredIntoRun(t *testing.T) {
	// A provider that always emits a (looping-distinct) tool call so the
	// agent keeps appending rounds until maxSteps.
	tp := &toolLoopProvider{}
	summ := &stubSummarizer{
		summary: "bounded",
		usage:   llm.TokenUsage{InputTokens: 7, OutputTokens: 1},
	}
	comp := NewRollingCompactor(summ, 2, 200, 50)
	comp.sync = true // deterministic in-test compaction

	ag, err := New(tp, []Tool{{
		Name:        "noop",
		Description: "does nothing",
		Run:         func(map[string]any) (string, error) { return strings.Repeat("padding ", 80), nil },
	}}, WithCompactor(comp))
	if err != nil {
		t.Fatal(err)
	}

	_, _, runErr := ag.Run(context.Background(), "", "do the long thing", 12)
	if runErr == nil {
		t.Fatal("expected the non-finalizing loop to exhaust maxSteps")
	}
	if summ.callCount() == 0 {
		t.Error("compactor never summarized during a long bounded loop")
	}
	// The summarizer's spend is folded into LastUsage.
	if ag.LastUsage().InputTokens < 7 {
		t.Errorf("summarizer usage not folded into LastUsage: %+v", ag.LastUsage())
	}
}

// toolLoopProvider emits a distinct tool call every step (varying args so the
// loop-detector does not short-circuit), never final_answer.
type toolLoopProvider struct{ n int }

func (p *toolLoopProvider) Name() string { return "loop" }
func (p *toolLoopProvider) Close() error { return nil }
func (p *toolLoopProvider) Complete(_ context.Context, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
	p.n++
	return llm.CompletionResponse{
		Text:  `{"tool":"noop","args":{"i":` + itoa(p.n) + `}}`,
		Usage: llm.TokenUsage{InputTokens: 50, OutputTokens: 5},
	}, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

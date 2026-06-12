package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/tokens"
)

// summarySentinel prefixes a synthetic frozen-zone note so partitionZones
// can recognise an already-compacted summary on a later turn and keep it
// in the frozen zone (never re-fold it into the compress zone).
const summarySentinel = "[rolling-summary] "

// Default tuning for the rolling compactor. ActiveRounds rounds (an
// assistant tool-call + its tool observation each) at the tail of the
// conversation are always kept verbatim; compaction triggers at
// CompressTriggerTokens and targets CompressTargetTokens for the folded
// summary.
const (
	defaultActiveRounds          = 4
	defaultCompressTriggerTokens = 12000
	defaultCompressTargetTokens  = 1500
)

// RollingCompactor bounds a long agent tool-loop's conversation by folding
// the middle ("compress") zone of rounds into a single synthetic frozen
// summary message when the conversation grows past a high-water mark.
//
// Three zones (oldest → newest, after the pinned system prompt + first
// user task):
//
//	frozen   — already-summarized rounds, carried as one synthetic
//	           RoleAssistant note (summarySentinel-prefixed).
//	compress — middle rounds eligible to be folded into the rolling summary.
//	active   — the most recent ActiveRounds rounds, kept verbatim.
//
// The system prompt (conv[0]) and the first user task (conv[1]) are always
// pinned and never enter any zone.
//
// A nil summarizer disables compaction entirely (Run behaves exactly as it
// did before the compactor existed) — that keeps WithCompactor backward
// compatible.
type RollingCompactor struct {
	ActiveRounds          int
	CompressTriggerTokens int
	CompressTargetTokens  int

	summarizer llm.Provider

	// sync forces synchronous-but-throttled compaction. When the agent's
	// Run deadline is too short to safely outlive a background summarizer
	// round (so the derived ctx would routinely cancel summaries mid-flight),
	// the compactor summarizes inline on the compaction turn instead of
	// spawning a goroutine. The agent loop already has a bounded step count,
	// so a synchronous summarize is bounded.
	sync bool

	// pending guards the in-flight / completed async summary state.
	pendingMu sync.Mutex
	inflight  bool   // a background summarizer goroutine is running
	ready     bool   // a completed summary is waiting to be spliced
	summary   string // the completed summary text (valid only when ready)

	// wg tracks any spawned summarizer goroutine so a returning Run can
	// wait it out (after cancelling its ctx) — that guarantees no
	// goroutine and no usage attribution leaks across Run boundaries.
	wg sync.WaitGroup
}

// NewRollingCompactor builds a compactor over a summarization provider.
// Non-positive tuning values fall back to the package defaults. A nil
// summarizer yields a no-op compactor (compaction disabled).
func NewRollingCompactor(summarizer llm.Provider, activeRounds, trigger, target int) *RollingCompactor {
	if activeRounds <= 0 {
		activeRounds = defaultActiveRounds
	}
	if trigger <= 0 {
		trigger = defaultCompressTriggerTokens
	}
	if target <= 0 {
		target = defaultCompressTargetTokens
	}
	return &RollingCompactor{
		ActiveRounds:          activeRounds,
		CompressTriggerTokens: trigger,
		CompressTargetTokens:  target,
		summarizer:            summarizer,
	}
}

// AgentOption mutates an Agent at construction time. The variadic option
// set lets New stay backward compatible — an Agent built with no options
// behaves exactly as before.
type AgentOption func(*Agent)

// WithCompactor installs a rolling-summary compactor on the agent. A nil
// compactor (or one with a nil summarizer) leaves the agent's Run identical
// to its un-compacted behaviour.
func WithCompactor(c *RollingCompactor) AgentOption {
	return func(a *Agent) { a.compactor = c }
}

// WithSyncCompaction is a test/throttling seam that forces synchronous
// (inline) summarization. Used when the run deadline is too short for the
// async path. It is exposed as an option so callers that already know their
// per-call deadline is tight can opt in without reflection.
func WithSyncCompaction() AgentOption {
	return func(a *Agent) {
		if a.compactor != nil {
			a.compactor.sync = true
		}
	}
}

// enabled reports whether the compactor can actually compact (it has a
// summarizer). A nil receiver or nil summarizer disables compaction.
func (c *RollingCompactor) enabled() bool {
	return c != nil && c.summarizer != nil
}

// wait blocks until any spawned background summarizer goroutine has
// returned. Run calls it (after cancelling the summarizer's ctx) so no
// goroutine — and no usage attribution — leaks past the Run that started it.
func (c *RollingCompactor) wait() {
	if c == nil {
		return
	}
	c.wg.Wait()
}

// partitionZones splits a running conversation into frozen / compress /
// active zones. The system prompt (conv[0]) and the first user task
// (conv[1]) are pinned out of every zone and are NOT returned here — the
// caller re-attaches them. Already-summarized frozen notes (recognised by
// summarySentinel) are carried into frozen and never re-compressed. The
// most recent activeRounds rounds (each round = an assistant message plus
// its following tool observation, i.e. two messages) stay in active,
// verbatim; everything between frozen and active is compress.
func partitionZones(conv []llm.Message, activeRounds int) (frozen, compress, active []llm.Message) {
	if len(conv) <= 2 {
		return nil, nil, nil
	}
	body := conv[2:] // drop pinned system + first user task

	// Leading frozen summaries stay frozen.
	i := 0
	for i < len(body) && isFrozenSummary(body[i]) {
		frozen = append(frozen, body[i])
		i++
	}
	rest := body[i:]

	// The tail activeRounds rounds (2 messages each) are kept verbatim.
	activeMsgs := activeRounds * 2
	if activeMsgs < 0 {
		activeMsgs = 0
	}
	if activeMsgs >= len(rest) {
		// Everything that isn't frozen still fits in active.
		active = append(active, rest...)
		return frozen, nil, active
	}
	split := len(rest) - activeMsgs
	compress = append(compress, rest[:split]...)
	active = append(active, rest[split:]...)
	return frozen, compress, active
}

// isFrozenSummary reports whether m is a synthetic rolling-summary note.
func isFrozenSummary(m llm.Message) bool {
	return m.Role == llm.RoleAssistant && strings.HasPrefix(m.Content, summarySentinel)
}

// countRounds returns how many (assistant, tool) round pairs the zone holds.
// A trailing lone assistant message counts as a partial round.
func countRounds(zone []llm.Message) int {
	return (len(zone) + 1) / 2
}

// estimateConvTokens sizes the conversation. When the most recent provider
// usage reports a nonzero prompt-token count it is preferred (it is the
// authoritative count of what the provider actually billed for the whole
// conversation prefix); otherwise the size is summed from the message text
// via tokens.Count.
func estimateConvTokens(conv []llm.Message, lastUsage llm.TokenUsage) int {
	if lastUsage.InputTokens > 0 {
		return lastUsage.InputTokens
	}
	total := 0
	for _, m := range conv {
		total += tokens.Count(m.Content)
	}
	return total
}

// maybeCompact folds the compress zone into a single frozen summary when the
// conversation has crossed the high-water mark and the compress zone holds at
// least two rounds. It returns the (possibly) compacted conversation.
//
// Cancellation / cost-attribution contract (decided in-spec):
//   - summCtx is derived from runCtx via context.WithCancel; the caller (Run)
//     cancels it on return (defer cancel()). So an in-flight summarizer call
//     cannot outlive the request.
//   - A summary that arrives after its context is done is DROPPED — the splice
//     site checks summCtx.Err()==nil before applying.
//   - The summarizer's own token usage is attributed: it is folded into the
//     passed *usage under mu, so background spend is never unaccounted.
//   - When sync==true (deadline too short for the async path) the summarize
//     runs inline; otherwise a single background goroutine is spawned and the
//     result is spliced on the next turn that finds it ready.
func (c *RollingCompactor) maybeCompact(runCtx context.Context, conv []llm.Message, sizeTokens int, usage *llm.TokenUsage, mu *sync.Mutex) (compacted []llm.Message, didCompact bool) {
	if !c.enabled() {
		return conv, false
	}

	// First, splice in any async summary that has completed since the last
	// turn (and whose context was not cancelled before completion).
	if spliced, ok := c.applyReadySummary(conv); ok {
		return spliced, true
	}

	if sizeTokens < c.CompressTriggerTokens {
		return conv, false
	}

	frozen, compress, active := partitionZones(conv, c.ActiveRounds)
	if countRounds(compress) < 2 {
		return conv, false // nothing eligible to fold yet
	}

	// Synchronous path: summarize inline and splice now.
	if c.sync {
		summCtx, cancel := context.WithCancel(runCtx)
		defer cancel()
		summary, u, err := c.summarizeZone(summCtx, compress)
		// Attribute the summarizer's spend regardless of splice outcome.
		mu.Lock()
		usage.Add(u)
		mu.Unlock()
		if err != nil || summCtx.Err() != nil || strings.TrimSpace(summary) == "" {
			return conv, false
		}
		return c.spliceSummary(conv, frozen, active, summary), true
	}

	// Async path: spawn a single background summarizer. The next turn that
	// finds the result ready splices it (above).
	c.spawnSummarize(runCtx, compress, usage, mu)
	return conv, false
}

// spawnSummarize launches at most one background summarizer goroutine on a
// context derived from runCtx. Run cancels that ctx on return; a summary
// produced after cancellation is dropped (its usage is still attributed).
func (c *RollingCompactor) spawnSummarize(runCtx context.Context, compress []llm.Message, usage *llm.TokenUsage, mu *sync.Mutex) {
	c.pendingMu.Lock()
	if c.inflight || c.ready {
		c.pendingMu.Unlock()
		return // a summarizer is already running or a result is pending
	}
	c.inflight = true
	c.pendingMu.Unlock()

	zone := append([]llm.Message(nil), compress...) // snapshot — caller may mutate conv

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		summCtx, cancel := context.WithCancel(runCtx)
		defer cancel()
		summary, u, err := c.summarizeZone(summCtx, zone)

		// Attribute spend even when the result is dropped — background
		// tokens are never unaccounted.
		mu.Lock()
		usage.Add(u)
		mu.Unlock()

		c.pendingMu.Lock()
		c.inflight = false
		// Drop a summary whose context was cancelled before it completed,
		// or an empty/failed one. A dropped summary leaves the conversation
		// unchanged on the next turn.
		if err == nil && summCtx.Err() == nil && strings.TrimSpace(summary) != "" {
			c.summary = summary
			c.ready = true
		}
		c.pendingMu.Unlock()
	}()
}

// applyReadySummary splices a completed (non-cancelled) async summary into
// the conversation, if one is waiting. It re-partitions the current
// conversation so the splice reflects whatever rounds have been appended
// since the summarizer was launched.
func (c *RollingCompactor) applyReadySummary(conv []llm.Message) ([]llm.Message, bool) {
	c.pendingMu.Lock()
	if !c.ready {
		c.pendingMu.Unlock()
		return conv, false
	}
	summary := c.summary
	c.ready = false
	c.summary = ""
	c.pendingMu.Unlock()

	frozen, compress, active := partitionZones(conv, c.ActiveRounds)
	if countRounds(compress) < 2 {
		// The compress zone shrank below the fold threshold since the
		// summarizer launched (unlikely — the conversation only grows).
		// Drop the stale summary rather than fold a single round.
		return conv, false
	}
	return c.spliceSummary(conv, frozen, active, summary), true
}

// spliceSummary rebuilds the conversation as: pinned (system + first task) +
// existing frozen notes + one new frozen summary + active rounds. The
// compress zone is dropped — it is now represented by the summary.
func (c *RollingCompactor) spliceSummary(conv, frozen, active []llm.Message, summary string) []llm.Message {
	out := make([]llm.Message, 0, 2+len(frozen)+1+len(active))
	out = append(out, conv[0], conv[1]) // pinned system + first user task
	out = append(out, frozen...)
	out = append(out, llm.Message{
		Role:    llm.RoleAssistant,
		Content: summarySentinel + summary,
	})
	out = append(out, active...)
	return out
}

// summarizeZone asks the summarizer provider to compress the compress-zone
// rounds into a concise note targeting CompressTargetTokens. It returns the
// note text and the provider's own token usage (for cost attribution).
func (c *RollingCompactor) summarizeZone(ctx context.Context, rounds []llm.Message) (string, llm.TokenUsage, error) {
	var b strings.Builder
	for _, m := range rounds {
		switch m.Role {
		case llm.RoleAssistant:
			b.WriteString("ASSISTANT (tool call): ")
		case llm.RoleTool:
			fmt.Fprintf(&b, "TOOL RESULT (%s): ", m.ToolName)
		case llm.RoleUser:
			b.WriteString("USER: ")
		default:
			b.WriteString(string(m.Role))
			b.WriteString(": ")
		}
		b.WriteString(m.Content)
		b.WriteString("\n")
	}

	prompt := fmt.Sprintf(
		"Summarize the following earlier steps of an agent tool-using session into a"+
			" compact note of at most %d tokens. Preserve the concrete facts, symbol ids,"+
			" file paths, and intermediate conclusions a later turn would need; drop"+
			" redundant tool chatter. Output only the summary text.\n\n%s",
		c.CompressTargetTokens, b.String(),
	)

	resp, err := c.summarizer.Complete(ctx, llm.CompletionRequest{
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: prompt}},
		MaxTokens: c.CompressTargetTokens,
		Shape:     llm.ShapeFreeform,
	})
	if err != nil {
		return "", resp.Usage, err
	}
	return strings.TrimSpace(resp.Text), resp.Usage, nil
}

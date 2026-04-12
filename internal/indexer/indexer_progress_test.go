package indexer

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/progress"
)

type reporterEvent struct {
	stage          string
	current, total int
}

type recordingReporter struct {
	mu     sync.Mutex
	events []reporterEvent
}

func (r *recordingReporter) Report(stage string, current, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, reporterEvent{stage, current, total})
}

func (r *recordingReporter) stages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, e := range r.events {
		if !seen[e.stage] {
			seen[e.stage] = true
			out = append(out, e.stage)
		}
	}
	return out
}

// TestIndexCtx_ReportsStages verifies the main indexer phases each emit at
// least one progress tick so the MCP reporter can translate them into
// notifications/progress messages.
func TestIndexCtx_ReportsStages(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), `package main

func A() {}
`)
	writeFile(t, filepath.Join(dir, "b.go"), `package main

func B() { A() }
`)

	g := graph.New()
	idx := newTestIndexer(g)
	rec := &recordingReporter{}
	ctx := progress.WithReporter(context.Background(), rec)

	result, err := idx.IndexCtx(ctx, dir)
	require.NoError(t, err)
	require.NotNil(t, result)

	stages := rec.stages()
	// The following stages are load-bearing for a usable progress story —
	// clients render these messages in the UI and users rely on them to see
	// that indexing is making forward progress through its phases.
	wantStages := []string{
		"walking files",
		"parsing",
		"resolving references",
		"inferring interfaces",
		"building search index",
		"extracting contracts",
		"indexing complete",
	}
	for _, s := range wantStages {
		assert.Contains(t, stages, s, "missing progress stage %q; got: %v", s, stages)
	}
}

// TestIndex_NoProgress_StillWorks verifies the backwards-compatible Index()
// entry point runs without a reporter attached.
func TestIndex_NoProgress_StillWorks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), `package main

func A() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	result, err := idx.Index(dir)
	require.NoError(t, err)
	assert.Equal(t, 1, result.FileCount)
}

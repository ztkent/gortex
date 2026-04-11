package indexer

import (
	"runtime"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// =============================================================================
// Indexer benchmarks with memory profiling for RPi / low-resource devices
// =============================================================================

// BenchmarkIndex_Self_Memory measures heap allocation during self-indexing.
func BenchmarkIndex_Self_Memory(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		b.StartTimer()

		g := graph.New()
		reg := parser.NewRegistry()
		languages.RegisterAll(reg)
		idx := New(g, reg, config.IndexConfig{}, zap.NewNop())
		stats, err := idx.Index("../..")
		if err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc), "heap-bytes")
		b.ReportMetric(float64(stats.FileCount), "files")
		b.ReportMetric(float64(g.NodeCount()), "nodes")
		b.ReportMetric(float64(g.EdgeCount()), "edges")
		b.StartTimer()
	}
}

// BenchmarkIndex_Self_Incremental measures incremental reindex performance.
// On RPi, incremental updates are the primary use case (full reindex is slow).
func BenchmarkIndex_Self_Incremental(b *testing.B) {
	// First, do a full index.
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, config.IndexConfig{}, zap.NewNop())
	_, err := idx.Index("../..")
	if err != nil {
		b.Fatal(err)
	}

	// Benchmark incremental reindex (detects stale files and re-indexes them).
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		idx.IncrementalReindex("../..")
	}
}

// BenchmarkIndex_Self_SingleFile measures single-file indexing latency.
func BenchmarkIndex_Self_SingleFile(b *testing.B) {
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, config.IndexConfig{}, zap.NewNop())

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		idx.IndexFile("../../internal/mcp/server.go")
	}
}

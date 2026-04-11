package analysis

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// =============================================================================
// Memory-focused analysis benchmarks for low-resource devices (RPi)
// =============================================================================

// buildSyntheticGraphForMem creates a graph without b.Helper for standalone use.
func buildSyntheticGraphForMem(nodeCount int) *graph.Graph {
	g := graph.New()
	for i := range nodeCount {
		g.AddNode(&graph.Node{
			ID:        fmt.Sprintf("pkg/file%d.go::Func%d", i/100, i),
			Kind:      graph.KindFunction,
			Name:      fmt.Sprintf("Func%d", i),
			FilePath:  fmt.Sprintf("pkg/file%d.go", i/100),
			StartLine: (i % 100) + 1,
			EndLine:   (i % 100) + 10,
			Language:  "go",
		})
	}
	for i := range nodeCount {
		t1 := (i + 1) % nodeCount
		t2 := (i + 7) % nodeCount
		g.AddEdge(&graph.Edge{
			From: fmt.Sprintf("pkg/file%d.go::Func%d", i/100, i),
			To:   fmt.Sprintf("pkg/file%d.go::Func%d", t1/100, t1),
			Kind: graph.EdgeCalls,
		})
		g.AddEdge(&graph.Edge{
			From: fmt.Sprintf("pkg/file%d.go::Func%d", i/100, i),
			To:   fmt.Sprintf("pkg/file%d.go::Func%d", t2/100, t2),
			Kind: graph.EdgeCalls,
		})
	}
	return g
}

// BenchmarkDetectCommunities_Memory measures heap allocation for community detection.
func BenchmarkDetectCommunities_Memory(b *testing.B) {
	sizes := []struct {
		name  string
		nodes int
	}{
		{"500_nodes", 500},
		{"2K_nodes", 2_000},
		{"5K_nodes", 5_000},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			g := buildSyntheticGraphForMem(sz.nodes)
			b.ResetTimer()
			for b.Loop() {
				DetectCommunities(g)
			}
		})
	}
}

// BenchmarkDiscoverProcesses_Memory measures heap allocation for process discovery.
func BenchmarkDiscoverProcesses_Memory(b *testing.B) {
	sizes := []struct {
		name  string
		nodes int
	}{
		{"500_nodes", 500},
		{"2K_nodes", 2_000},
		{"5K_nodes", 5_000},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			g := buildSyntheticGraphForMem(sz.nodes)
			b.ResetTimer()
			for b.Loop() {
				DiscoverProcesses(g)
			}
		})
	}
}

// BenchmarkAnalyzeImpact_Memory measures impact analysis memory usage.
func BenchmarkAnalyzeImpact_Memory(b *testing.B) {
	sizes := []struct {
		name  string
		nodes int
	}{
		{"500_nodes", 500},
		{"2K_nodes", 2_000},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			g := buildSyntheticGraphForMem(sz.nodes)
			symbolIDs := []string{
				"pkg/file0.go::Func0",
				"pkg/file1.go::Func100",
			}
			b.ResetTimer()
			for b.Loop() {
				AnalyzeImpact(g, symbolIDs, nil, nil)
			}
		})
	}
}

// BenchmarkAnalysis_HeapFootprint measures total heap consumed by each analysis pass.
func BenchmarkAnalysis_HeapFootprint(b *testing.B) {
	g := buildSyntheticGraphForMem(3000)

	analyses := []struct {
		name string
		fn   func()
	}{
		{"DetectCommunities", func() { DetectCommunities(g) }},
		{"DiscoverProcesses", func() { DiscoverProcesses(g) }},
		{"AnalyzeImpact", func() { AnalyzeImpact(g, []string{"pkg/file0.go::Func0"}, nil, nil) }},
		{"FindDeadCode", func() { FindDeadCode(g, nil, nil) }},
	}

	for _, a := range analyses {
		b.Run(a.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				runtime.GC()
				var before runtime.MemStats
				runtime.ReadMemStats(&before)
				b.StartTimer()

				a.fn()

				b.StopTimer()
				runtime.GC()
				var after runtime.MemStats
				runtime.ReadMemStats(&after)
				b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc), "total-alloc-bytes")
				b.StartTimer()
			}
		})
	}
}

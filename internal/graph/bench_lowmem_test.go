package graph

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// =============================================================================
// Scaled graph benchmarks — simulate RPi memory constraints
// =============================================================================

// graphSizes defines graph sizes for sub-benchmarks.
// "Tiny" approximates a small project on RPi, "Large" approximates a medium monorepo.
var graphSizes = []struct {
	name  string
	nodes int
	edges int
}{
	{"Tiny_100", 100, 200},
	{"Small_1K", 1_000, 3_000},
	{"Medium_5K", 5_000, 15_000},
	{"Large_10K", 10_000, 30_000},
	{"XL_50K", 50_000, 150_000},
	{"XXL_100K", 100_000, 300_000},
}

func buildScaledGraph(nodes, edges int) *Graph {
	g := New()
	for i := range nodes {
		g.AddNode(&Node{
			ID:       fmt.Sprintf("pkg%d/file%d.go::sym%d", i/100, i/10, i),
			Kind:     KindFunction,
			Name:     fmt.Sprintf("sym%d", i),
			FilePath: fmt.Sprintf("pkg%d/file%d.go", i/100, i/10),
			Language: "go",
		})
	}
	for i := range edges {
		g.AddEdge(&Edge{
			From: fmt.Sprintf("pkg%d/file%d.go::sym%d", (i%nodes)/100, (i%nodes)/10, i%nodes),
			To:   fmt.Sprintf("pkg%d/file%d.go::sym%d", ((i+1)%nodes)/100, ((i+1)%nodes)/10, (i+1)%nodes),
			Kind: EdgeCalls,
		})
	}
	return g
}

func BenchmarkGraph_AddNode_Scaled(b *testing.B) {
	for _, sz := range graphSizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			g := New()
			b.ResetTimer()
			for i := range b.N {
				g.AddNode(&Node{
					ID:   fmt.Sprintf("file%d.go::func%d", i/10, i),
					Kind: KindFunction,
					Name: fmt.Sprintf("func%d", i),
				})
			}
		})
	}
}

func BenchmarkGraph_GetNode_Scaled(b *testing.B) {
	for _, sz := range graphSizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			g := buildScaledGraph(sz.nodes, sz.edges)
			b.ResetTimer()
			for i := range b.N {
				g.GetNode(fmt.Sprintf("pkg%d/file%d.go::sym%d", (i%sz.nodes)/100, (i%sz.nodes)/10, i%sz.nodes))
			}
		})
	}
}

func BenchmarkGraph_AllNodes_Scaled(b *testing.B) {
	for _, sz := range graphSizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			g := buildScaledGraph(sz.nodes, sz.edges)
			b.ResetTimer()
			for b.Loop() {
				g.AllNodes()
			}
		})
	}
}

func BenchmarkGraph_Stats_Scaled(b *testing.B) {
	for _, sz := range graphSizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			g := buildScaledGraph(sz.nodes, sz.edges)
			b.ResetTimer()
			for b.Loop() {
				g.Stats()
			}
		})
	}
}

// =============================================================================
// Eviction benchmarks — critical for memory-constrained devices
// =============================================================================

func BenchmarkGraph_EvictFile(b *testing.B) {
	for _, sz := range graphSizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				g := buildScaledGraph(sz.nodes, sz.edges)
				b.StartTimer()
				// Evict ~10% of files
				for i := range sz.nodes / 10 {
					g.EvictFile(fmt.Sprintf("pkg%d/file%d.go", i/100, i/10))
				}
			}
		})
	}
}

// =============================================================================
// Concurrent access benchmarks — RPi has 4 cores, contention matters
// =============================================================================

func BenchmarkGraph_ConcurrentRead(b *testing.B) {
	for _, sz := range graphSizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			g := buildScaledGraph(sz.nodes, sz.edges)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					g.GetNode(fmt.Sprintf("pkg%d/file%d.go::sym%d", (i%sz.nodes)/100, (i%sz.nodes)/10, i%sz.nodes))
					i++
				}
			})
		})
	}
}

func BenchmarkGraph_ConcurrentReadWrite(b *testing.B) {
	for _, sz := range graphSizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			g := buildScaledGraph(sz.nodes, sz.edges)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					if i%10 == 0 {
						// 10% writes
						g.AddNode(&Node{
							ID:   fmt.Sprintf("new%d", i+sz.nodes),
							Kind: KindFunction,
							Name: fmt.Sprintf("newFunc%d", i),
						})
					} else {
						g.GetNode(fmt.Sprintf("pkg%d/file%d.go::sym%d", (i%sz.nodes)/100, (i%sz.nodes)/10, i%sz.nodes))
					}
					i++
				}
			})
		})
	}
}

// =============================================================================
// Memory footprint measurement
// =============================================================================

func BenchmarkGraph_MemoryFootprint(b *testing.B) {
	for _, sz := range graphSizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				runtime.GC()
				var before runtime.MemStats
				runtime.ReadMemStats(&before)
				b.StartTimer()

				g := buildScaledGraph(sz.nodes, sz.edges)
				_ = g.NodeCount() // prevent optimization

				b.StopTimer()
				runtime.GC()
				var after runtime.MemStats
				runtime.ReadMemStats(&after)
				b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc), "heap-bytes")
				b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/float64(sz.nodes), "bytes/node")
				b.StartTimer()
			}
		})
	}
}

// =============================================================================
// GC pressure benchmark — important for RPi's limited memory bandwidth
// =============================================================================

func BenchmarkGraph_GCPressure(b *testing.B) {
	b.ReportAllocs()
	g := buildScaledGraph(50_000, 150_000)

	// Simulate churn: add and evict files repeatedly
	b.ResetTimer()
	for i := range b.N {
		filePath := fmt.Sprintf("churn/file%d.go", i%100)
		g.EvictFile(filePath)
		for j := range 10 {
			g.AddNode(&Node{
				ID:       fmt.Sprintf("%s::func%d", filePath, j),
				Kind:     KindFunction,
				Name:     fmt.Sprintf("func%d", j),
				FilePath: filePath,
			})
		}
	}
}

// =============================================================================
// Lock contention benchmark — simulates RPi's 4-core scenario
// =============================================================================

func BenchmarkGraph_LockContention(b *testing.B) {
	for _, goroutines := range []int{2, 4, 8} {
		b.Run(fmt.Sprintf("goroutines_%d", goroutines), func(b *testing.B) {
			b.ReportAllocs()
			g := buildScaledGraph(1000, 3000)
			b.ResetTimer()

			var wg sync.WaitGroup
			opsPerGoroutine := b.N / goroutines
			if opsPerGoroutine == 0 {
				opsPerGoroutine = 1
			}

			for gr := range goroutines {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					for i := range opsPerGoroutine {
						switch i % 4 {
						case 0:
							g.GetNode(fmt.Sprintf("pkg%d/file%d.go::sym%d", (i%1000)/100, (i%1000)/10, i%1000))
						case 1:
							g.AllNodes()
						case 2:
							g.GetOutEdges(fmt.Sprintf("pkg%d/file%d.go::sym%d", (i%1000)/100, (i%1000)/10, i%1000))
						case 3:
							g.AddNode(&Node{
								ID:   fmt.Sprintf("g%d_n%d", id, i),
								Kind: KindVariable,
								Name: fmt.Sprintf("v%d", i),
							})
						}
					}
				}(gr)
			}
			wg.Wait()
		})
	}
}

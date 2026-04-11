package resolver

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// buildResolverGraph creates a graph with unresolved edges for benchmarking.
func buildResolverGraph(files, symsPerFile int) (*graph.Graph, *Resolver) {
	g := graph.New()

	// Create file nodes with functions, types, and methods.
	for f := range files {
		filePath := fmt.Sprintf("pkg%d/file%d.go", f/10, f)
		pkg := fmt.Sprintf("pkg%d", f/10)

		// Add a type per file.
		typeName := fmt.Sprintf("Type%d", f)
		g.AddNode(&graph.Node{
			ID:       fmt.Sprintf("%s::%s", filePath, typeName),
			Kind:     graph.KindType,
			Name:     typeName,
			QualName: fmt.Sprintf("%s.%s", pkg, typeName),
			FilePath: filePath,
			Language: "go",
			Meta:     map[string]any{"receiver_type": typeName},
		})

		for s := range symsPerFile {
			funcName := fmt.Sprintf("Func%d_%d", f, s)
			nodeID := fmt.Sprintf("%s::%s", filePath, funcName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Kind:     graph.KindFunction,
				Name:     funcName,
				QualName: fmt.Sprintf("%s.%s", pkg, funcName),
				FilePath: filePath,
				Language: "go",
			})

			// Add unresolved call edges to functions in other files.
			targetFile := (f + 1) % files
			targetFunc := fmt.Sprintf("Func%d_%d", targetFile, s%symsPerFile)
			g.AddEdge(&graph.Edge{
				From:     nodeID,
				To:       "unresolved::" + targetFunc,
				Kind:     graph.EdgeCalls,
				FilePath: filePath,
			})
		}
	}
	return g, New(g)
}

func BenchmarkResolver_ResolveAll(b *testing.B) {
	sizes := []struct {
		name         string
		files        int
		symsPerFile  int
	}{
		{"Small_50files", 50, 5},
		{"Medium_200files", 200, 10},
		{"Large_500files", 500, 10},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				_, r := buildResolverGraph(sz.files, sz.symsPerFile)
				b.StartTimer()
				r.ResolveAll()
			}
		})
	}
}

func BenchmarkResolver_ResolveFile(b *testing.B) {
	_, r := buildResolverGraph(200, 10)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		r.ResolveFile(fmt.Sprintf("pkg%d/file%d.go", (i%200)/10, i%200))
	}
}

func BenchmarkResolver_InferImplements(b *testing.B) {
	g := graph.New()

	// Create interfaces and implementing types.
	for i := range 20 {
		ifaceName := fmt.Sprintf("Interface%d", i)
		g.AddNode(&graph.Node{
			ID:       fmt.Sprintf("pkg/iface.go::%s", ifaceName),
			Kind:     graph.KindInterface,
			Name:     ifaceName,
			FilePath: "pkg/iface.go",
			Language: "go",
			Meta: map[string]any{
				"methods": []string{fmt.Sprintf("Method%d", i), fmt.Sprintf("Other%d", i)},
			},
		})

		// 5 types implement each interface.
		for j := range 5 {
			typeName := fmt.Sprintf("Impl%d_%d", i, j)
			filePath := fmt.Sprintf("pkg/impl%d.go", j)
			g.AddNode(&graph.Node{
				ID:       fmt.Sprintf("%s::%s", filePath, typeName),
				Kind:     graph.KindType,
				Name:     typeName,
				FilePath: filePath,
				Language: "go",
				Meta:     map[string]any{"receiver_type": typeName},
			})
			// Add methods matching the interface.
			for _, mName := range []string{fmt.Sprintf("Method%d", i), fmt.Sprintf("Other%d", i)} {
				methodID := fmt.Sprintf("%s::%s.%s", filePath, typeName, mName)
				g.AddNode(&graph.Node{
					ID:       methodID,
					Kind:     graph.KindMethod,
					Name:     mName,
					FilePath: filePath,
					Language: "go",
					Meta:     map[string]any{"receiver_type": typeName},
				})
			}
		}
	}

	r := New(g)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r.InferImplements()
	}
}

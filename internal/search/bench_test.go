package search

import (
	"fmt"
	"testing"
)

// =============================================================================
// Tokenizer benchmarks — hot path for every search query
// =============================================================================

var tokenizeInputs = []struct {
	name  string
	input string
}{
	{"CamelCase", "getUserByIdFromDatabase"},
	{"SnakeCase", "get_user_by_id_from_database"},
	{"DotPath", "internal/mcp/server.go::NewServer"},
	{"ALLCAPS", "HTMLParserFactory"},
	{"Mixed", "parseJSON_toXML.Convert"},
	{"Long", "internal/parser/languages/typescript.go::TypeScriptExtractor.extractClassDeclaration"},
}

func BenchmarkTokenize(b *testing.B) {
	for _, tc := range tokenizeInputs {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				Tokenize(tc.input)
			}
		})
	}
}

func BenchmarkTokenizeQuery(b *testing.B) {
	queries := []string{
		"get user auth",
		"Server NewServer handle request",
		"graph add node edge",
		"resolve import cross repo",
	}
	for _, q := range queries {
		b.Run(q, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				TokenizeQuery(q)
			}
		})
	}
}

// =============================================================================
// BM25 scaled benchmarks — memory and latency at different corpus sizes
// =============================================================================

func buildBM25(size int) *BM25Backend {
	b := NewBM25()
	for i := range size {
		b.Add(
			fmt.Sprintf("pkg%d/file%d.go::func%d", i/100, i/10, i),
			fmt.Sprintf("getUserById%d", i%50),
			fmt.Sprintf("internal/pkg%d/service%d.go", i/100, i/10),
			fmt.Sprintf("func getUserById%d(id string) User", i%50),
		)
	}
	return b
}

func BenchmarkBM25_Search_Scaled(b *testing.B) {
	sizes := []struct {
		name string
		n    int
	}{
		{"100_symbols", 100},
		{"1K_symbols", 1_000},
		{"5K_symbols", 5_000},
		{"10K_symbols", 10_000},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			backend := buildBM25(sz.n)
			b.ResetTimer()
			for b.Loop() {
				backend.Search("get user auth", 20)
			}
		})
	}
}

func BenchmarkBM25_Add(b *testing.B) {
	b.ReportAllocs()
	backend := NewBM25()
	for i := range b.N {
		backend.Add(
			fmt.Sprintf("file%d.go::func%d", i/10, i),
			fmt.Sprintf("processRequest%d", i),
			fmt.Sprintf("pkg/file%d.go", i/10),
			fmt.Sprintf("func processRequest%d()", i),
		)
	}
}

func BenchmarkBM25_Remove(b *testing.B) {
	backend := buildBM25(5000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		backend.Remove(fmt.Sprintf("pkg%d/file%d.go::func%d", (i%5000)/100, (i%5000)/10, i%5000))
	}
}

func BenchmarkBM25_ConcurrentSearch(b *testing.B) {
	backend := buildBM25(5000)
	queries := []string{"get user", "server handle", "graph node", "parse extract", "resolve import"}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			backend.Search(queries[i%len(queries)], 20)
			i++
		}
	})
}

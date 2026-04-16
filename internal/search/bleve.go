package search

import (
	"strings"
	"sync/atomic"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/analysis/tokenizer/unicode"
	"github.com/blevesearch/bleve/v2/mapping"

	// Register default KV store.
	_ "github.com/blevesearch/bleve/v2/index/upsidedown/store/gtreap"
)

// BleveBackend wraps Bleve for full-text search over code symbols.
// Better for large repos (50k+ symbols) and multi-repo mode.
type BleveBackend struct {
	index bleve.Index
	count atomic.Int64
}

// symbolDoc is the document structure indexed by Bleve.
type symbolDoc struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Signature string `json:"signature"`
	// Combined field for broader matching.
	All string `json:"all"`
}

// NewBleve creates a Bleve-backed search index (in-memory).
func NewBleve() (*BleveBackend, error) {
	indexMapping := buildMapping()

	idx, err := bleve.NewMemOnly(indexMapping)
	if err != nil {
		return nil, err
	}

	return &BleveBackend{index: idx}, nil
}

func buildMapping() *mapping.IndexMappingImpl {
	indexMapping := bleve.NewIndexMapping()

	// Custom analyzer: unicode tokenizer + lowercase.
	// We pre-tokenize camelCase in Add(), so the analyzer just needs
	// to handle the space-separated tokens we give it.
	err := indexMapping.AddCustomAnalyzer("code", map[string]any{
		"type":      custom.Name,
		"tokenizer": unicode.Name,
		"token_filters": []string{
			lowercase.Name,
		},
	})
	if err != nil {
		// Fallback to default analyzer.
		return indexMapping
	}

	// Document mapping.
	docMapping := bleve.NewDocumentMapping()

	nameField := bleve.NewTextFieldMapping()
	nameField.Analyzer = "code"
	nameField.Store = false
	docMapping.AddFieldMappingsAt("name", nameField)

	pathField := bleve.NewTextFieldMapping()
	pathField.Analyzer = "code"
	pathField.Store = false
	docMapping.AddFieldMappingsAt("path", pathField)

	sigField := bleve.NewTextFieldMapping()
	sigField.Analyzer = "code"
	sigField.Store = false
	docMapping.AddFieldMappingsAt("signature", sigField)

	allField := bleve.NewTextFieldMapping()
	allField.Analyzer = "code"
	allField.Store = false
	docMapping.AddFieldMappingsAt("all", allField)

	indexMapping.DefaultMapping = docMapping
	indexMapping.DefaultAnalyzer = "code"

	return indexMapping
}

func (b *BleveBackend) Add(id string, fields ...string) {
	// Pre-tokenize camelCase and rejoin with spaces so Bleve's
	// unicode tokenizer can split them.
	var parts []string
	for _, f := range fields {
		tokens := Tokenize(f)
		parts = append(parts, strings.Join(tokens, " "))
	}

	doc := symbolDoc{
		All: strings.Join(parts, " "),
	}
	if len(parts) > 0 {
		doc.Name = parts[0]
	}
	if len(parts) > 1 {
		doc.Path = parts[1]
	}
	if len(parts) > 2 {
		doc.Signature = parts[2]
	}

	if err := b.index.Index(id, doc); err == nil {
		b.count.Add(1)
	}
}

func (b *BleveBackend) Remove(id string) {
	if err := b.index.Delete(id); err == nil {
		b.count.Add(-1)
	}
}

func (b *BleveBackend) Search(query string, limit int) []SearchResult {
	// Pre-tokenize the query for camelCase splitting.
	tokens := TokenizeQuery(query)
	if len(tokens) == 0 {
		return nil
	}
	q := strings.Join(tokens, " ")

	searchReq := bleve.NewSearchRequest(bleve.NewQueryStringQuery(q))
	searchReq.Size = limit

	res, err := b.index.Search(searchReq)
	if err != nil || res.Total == 0 {
		return nil
	}

	out := make([]SearchResult, 0, len(res.Hits))
	for _, hit := range res.Hits {
		out = append(out, SearchResult{
			ID:    hit.ID,
			Score: hit.Score,
		})
	}
	return out
}

func (b *BleveBackend) Count() int {
	return int(b.count.Load())
}

// SizeBytes approximates Bleve's in-memory footprint. Bleve doesn't
// expose a direct byte size, so we scale by document count: ~2 KB per
// indexed symbol covers the tokenised term dictionary, posting lists,
// and stored fields for the symbolDoc structure we write. The
// constant was calibrated against a 50k-symbol index on a typical Go
// repo (~100 MiB) — within a factor of ~1.5× of actual.
func (b *BleveBackend) SizeBytes() uint64 {
	return uint64(b.count.Load()) * 2048
}

func (b *BleveBackend) Close() {
	if b.index != nil {
		_ = b.index.Close()
	}
}

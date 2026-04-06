package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/resolver"
)

// IndexResult holds the outcome of an indexing operation.
type IndexResult struct {
	NodeCount  int          `json:"node_count"`
	EdgeCount  int          `json:"edge_count"`
	FileCount  int          `json:"file_count"`
	DurationMs int64        `json:"duration_ms"`
	Errors     []IndexError `json:"errors,omitempty"`
}

// IndexError records a per-file parsing failure.
type IndexError struct {
	FilePath string `json:"file_path"`
	Error    string `json:"error"`
}

// Indexer walks a repository and populates the graph.
type Indexer struct {
	graph    *graph.Graph
	registry *parser.Registry
	resolver *resolver.Resolver
	config   config.IndexConfig
	rootPath string
	logger   *zap.Logger
}

// New creates an Indexer.
func New(g *graph.Graph, reg *parser.Registry, cfg config.IndexConfig, logger *zap.Logger) *Indexer {
	return &Indexer{
		graph:    g,
		registry: reg,
		resolver: resolver.New(g),
		config:   cfg,
		logger:   logger,
	}
}

// Graph returns the underlying graph.
func (idx *Indexer) Graph() *graph.Graph { return idx.graph }

// RootPath returns the root path used for relative path computation.
func (idx *Indexer) RootPath() string { return idx.rootPath }

// Index walks root and populates the graph using a concurrent worker pool.
func (idx *Indexer) Index(root string) (*IndexResult, error) {
	start := time.Now()

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	idx.rootPath = absRoot

	// Collect files.
	var files []string
	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if idx.shouldExclude(path, absRoot) {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := idx.registry.DetectLanguage(path); ok {
			if !idx.shouldExclude(path, absRoot) {
				files = append(files, path)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Worker pool.
	workers := idx.config.Workers
	if workers <= 0 {
		workers = 1
	}

	type fileResult struct {
		nodes []*graph.Node
		edges []*graph.Edge
		err   error
		file  string
	}

	fileCh := make(chan string, len(files))
	resultCh := make(chan fileResult, len(files))

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range fileCh {
				fr := fileResult{file: path}
				src, err := os.ReadFile(path)
				if err != nil {
					fr.err = err
					resultCh <- fr
					continue
				}

				relPath, _ := filepath.Rel(absRoot, path)
				lang, _ := idx.registry.DetectLanguage(path)
				ext, _ := idx.registry.GetByLanguage(lang)
				if ext == nil {
					continue
				}

				result, err := ext.Extract(relPath, src)
				if err != nil {
					fr.err = err
					resultCh <- fr
					continue
				}
				fr.nodes = result.Nodes
				fr.edges = result.Edges
				resultCh <- fr
			}
		}()
	}

	for _, f := range files {
		fileCh <- f
	}
	close(fileCh)

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	var errors []IndexError
	fileCount := 0
	for fr := range resultCh {
		if fr.err != nil {
			errors = append(errors, IndexError{FilePath: fr.file, Error: fr.err.Error()})
			continue
		}
		fileCount++
		for _, n := range fr.nodes {
			idx.graph.AddNode(n)
		}
		for _, e := range fr.edges {
			idx.graph.AddEdge(e)
		}
	}

	// Resolve cross-file references.
	idx.resolver.ResolveAll()

	// Infer structural interface satisfaction.
	idx.resolver.InferImplements()

	return &IndexResult{
		NodeCount:  idx.graph.NodeCount(),
		EdgeCount:  idx.graph.EdgeCount(),
		FileCount:  fileCount,
		DurationMs: time.Since(start).Milliseconds(),
		Errors:     errors,
	}, nil
}

// IndexFile parses a single file and patches the graph (evict then add).
func (idx *Indexer) IndexFile(filePath string) error {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return err
	}

	relPath, err := filepath.Rel(idx.rootPath, absPath)
	if err != nil {
		relPath = filePath
	}

	// Evict existing data for this file.
	idx.graph.EvictFile(relPath)

	src, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}

	lang, ok := idx.registry.DetectLanguage(absPath)
	if !ok {
		return nil
	}
	ext, _ := idx.registry.GetByLanguage(lang)
	if ext == nil {
		return nil
	}

	result, err := ext.Extract(relPath, src)
	if err != nil {
		return err
	}

	for _, n := range result.Nodes {
		idx.graph.AddNode(n)
	}
	for _, e := range result.Edges {
		idx.graph.AddEdge(e)
	}

	idx.resolver.ResolveFile(relPath)
	return nil
}

// EvictFile removes all nodes and edges belonging to filePath.
func (idx *Indexer) EvictFile(filePath string) (int, int) {
	relPath, err := filepath.Rel(idx.rootPath, filePath)
	if err != nil {
		relPath = filePath
	}
	return idx.graph.EvictFile(relPath)
}

func (idx *Indexer) shouldExclude(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	// Normalize to forward slashes for pattern matching.
	rel = filepath.ToSlash(rel)

	for _, pattern := range idx.config.Exclude {
		// Simple directory-based exclusion.
		dir := strings.TrimSuffix(pattern, "/**")
		dir = strings.TrimSuffix(dir, "/*")
		dir = strings.TrimPrefix(dir, "**/")

		if strings.Contains(rel, dir+"/") || strings.HasPrefix(rel, dir+"/") || rel == dir {
			return true
		}
	}
	return false
}

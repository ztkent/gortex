package indexer

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// errExtractTimeout is the sentinel returned when a file's extraction
// exceeds IndexConfig.MaxExtractMillis.
var errExtractTimeout = errors.New("extraction exceeded time budget")

// skippedFile records a file dropped by the size cap, kept so a
// synthetic telemetry node can be emitted after the parse pass.
type skippedFile struct {
	relPath string
	lang    string
	size    int64
}

// walkedFile records a file that survived the walk-time filters,
// together with its walk-time ModTime so the worker and the
// post-parse fileMtimes loop don't need to re-stat. The walk does
// exactly one os.Stat per surviving file via d.Info(); everything
// downstream reads from this struct.
type walkedFile struct {
	path      string
	mtimeNano int64
}

// extractWithTimeout runs ext.Extract under the per-file extraction
// budget. With no budget configured it calls Extract directly. On
// timeout it returns errExtractTimeout; the slow extraction runs on to
// completion in its goroutine (tree-sitter's own 5s parse cap bounds
// the worst case) and its result is discarded.
func (idx *Indexer) extractWithTimeout(ext parser.Extractor, relPath string, src []byte) (*parser.ExtractionResult, error) {
	budget := idx.config.MaxExtractMillis
	if budget <= 0 {
		return ext.Extract(relPath, src)
	}
	type outcome struct {
		result *parser.ExtractionResult
		err    error
	}
	ch := make(chan outcome, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				ch <- outcome{err: fmt.Errorf("extractor panic: %v", rec)}
			}
		}()
		r, err := ext.Extract(relPath, src)
		ch <- outcome{result: r, err: err}
	}()
	timer := time.NewTimer(time.Duration(budget) * time.Millisecond)
	defer timer.Stop()
	select {
	case o := <-ch:
		return o.result, o.err
	case <-timer.C:
		return nil, errExtractTimeout
	}
}

// timeoutSkipResult builds a synthetic single-node result for a file
// whose extraction blew the time budget, so it stays visible in the
// graph with skip telemetry attached.
func timeoutSkipResult(relPath, lang string, budgetMS int) *parser.ExtractionResult {
	return &parser.ExtractionResult{
		Nodes: []*graph.Node{{
			ID:       relPath,
			Kind:     graph.KindFile,
			Name:     filepath.Base(relPath),
			FilePath: relPath,
			Language: lang,
			Meta: map[string]any{
				"skipped_due_to_timeout": true,
				"extract_budget_ms":      budgetMS,
			},
		}},
	}
}

// minifiedSkipResult builds a synthetic single-node result for a build
// artifact (a minified bundle or a sourcemap) detected by content and
// skipped, so it stays visible in the graph with the skip reason
// attached instead of polluting it with mangled symbols.
func minifiedSkipResult(relPath, lang, reason string) *parser.ExtractionResult {
	return &parser.ExtractionResult{
		Nodes: []*graph.Node{{
			ID:       relPath,
			Kind:     graph.KindFile,
			Name:     filepath.Base(relPath),
			FilePath: relPath,
			Language: lang,
			Meta: map[string]any{
				"skipped_due_to_minified": true,
				"minified_reason":         reason,
			},
		}},
	}
}

// sizeSkipNode builds a synthetic file node for a file dropped by the
// size cap.
func sizeSkipNode(sf skippedFile, maxSize int64) *graph.Node {
	return &graph.Node{
		ID:       sf.relPath,
		Kind:     graph.KindFile,
		Name:     filepath.Base(sf.relPath),
		FilePath: sf.relPath,
		Language: sf.lang,
		Meta: map[string]any{
			"skipped_due_to_size": true,
			"file_size_bytes":     sf.size,
			"max_file_size_bytes": maxSize,
		},
	}
}

// emitSizeSkipNodes adds synthetic file nodes for every size-skipped
// file so the file stays visible in the graph (queryable,
// get_file_summary works) with skip telemetry instead of vanishing.
func (idx *Indexer) emitSizeSkipNodes(skipped []skippedFile) {
	if len(skipped) == 0 {
		return
	}
	maxSize := idx.config.MaxFileSize
	nodes := make([]*graph.Node, 0, len(skipped))
	for _, sf := range skipped {
		nodes = append(nodes, sizeSkipNode(sf, maxSize))
	}
	idx.applyRepoPrefix(nodes, nil)
	idx.graph.AddBatch(nodes, nil)
}

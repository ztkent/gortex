package indexer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/crashpool"
)

// crashIsolationEnabled reports whether tree-sitter extraction should
// run in isolated worker subprocesses. GORTEX_PARSER_ISOLATION
// overrides the index.crash_isolation config key.
func (idx *Indexer) crashIsolationEnabled() bool {
	if v := os.Getenv("GORTEX_PARSER_ISOLATION"); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	return idx.config.CrashIsolation
}

// newParsePool spawns a crash-isolated parser pool — `workers` worker
// subprocesses, each a `gortex __parse-worker` instance. When a
// per-file extraction budget is configured it also bounds the worker
// round-trip so a hung file is killed on the same deadline.
func (idx *Indexer) newParsePool(workers int) (*crashpool.Pool, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cfg := crashpool.Config{
		Argv:    []string{exe, "__parse-worker"},
		Workers: workers,
		Logger:  idx.logger,
	}
	if ms := idx.config.MaxExtractMillis; ms > 0 {
		cfg.RequestTimeout = time.Duration(ms) * time.Millisecond
	}
	return crashpool.NewPool(cfg)
}

// sharedParsePool returns the long-lived crash-isolation pool and the
// quarantine, creating them on first use. Reusing one pool across
// single-file re-indexes avoids forking a worker subprocess per file
// on the watcher hot path — the dominant cost when crash isolation is
// on. Returns (nil, nil) if no worker can spawn, so the caller falls
// back to in-process extraction.
func (idx *Indexer) sharedParsePool() (*crashpool.Pool, *crashpool.Quarantine) {
	idx.parsePoolMu.Lock()
	defer idx.parsePoolMu.Unlock()
	if idx.parsePool != nil {
		return idx.parsePool, idx.parseQuar
	}
	// A long-lived pool keeps idle workers resident, so cap the worker
	// count well below the bulk-index width.
	workers := idx.config.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > 4 {
		workers = 4
	}
	p, err := idx.newParsePool(workers)
	if err != nil {
		idx.logger.Warn("indexer: crash-isolation pool unavailable; using in-process extraction",
			zap.Error(err))
		return nil, nil
	}
	idx.parsePool = p
	idx.parseQuar = crashpool.LoadQuarantine(
		filepath.Join(idx.rootPath, ".gortex", "parser-quarantine.json"))
	return idx.parsePool, idx.parseQuar
}

// CloseParsePool tears down the long-lived crash-isolation pool if one
// was created. It is safe to call when none exists, and idempotent.
func (idx *Indexer) CloseParsePool() {
	idx.parsePoolMu.Lock()
	defer idx.parsePoolMu.Unlock()
	if idx.parsePool != nil {
		idx.parsePool.Close()
		idx.parsePool = nil
	}
}

// extractFile produces one file's graph contribution. With pool == nil
// it calls the extractor in-process (the default). With a pool the
// parse runs in a worker subprocess: a crash, hang, or panic quarantines
// the file and yields a synthetic KindFile node carrying
// Meta["parse_error"] instead of aborting the whole index pass.
//
// The returned bool reports whether the file was skipped — quarantined
// after a crash, or skipped after blowing the extraction time budget.
// Callers MUST skip coverage + contract extraction for skipped files,
// since both re-parse the source and would re-trigger the fault.
func (idx *Indexer) extractFile(
	pool *crashpool.Pool, q *crashpool.Quarantine,
	path, relPath, lang string, ext parser.Extractor, src []byte,
) (result *parser.ExtractionResult, skipped bool, err error) {
	if pool == nil {
		r, eerr := idx.extractWithTimeout(ext, relPath, src)
		if errors.Is(eerr, errExtractTimeout) {
			budget := idx.config.MaxExtractMillis
			idx.logger.Warn("indexer: file extraction exceeded budget; skipped",
				zap.String("file", relPath), zap.Int("budget_ms", budget))
			return timeoutSkipResult(relPath, lang, budget), true,
				fmt.Errorf("extraction exceeded %dms budget: %s", budget, relPath)
		}
		if eerr != nil {
			return nil, false, eerr
		}
		stampParseErrors(r)
		return r, false, nil
	}

	var mtime int64
	if info, statErr := os.Stat(path); statErr == nil {
		mtime = info.ModTime().UnixNano()
	}
	if q.IsQuarantined(relPath, mtime) {
		return quarantineResult(relPath, lang, "skipped — file previously crashed the parser"),
			true, fmt.Errorf("skipped quarantined file %s", relPath)
	}

	res := pool.Submit(relPath, lang, src)
	switch {
	case res.Crashed || res.Panicked:
		q.Add(relPath, res.Err, mtime)
		idx.logger.Warn("indexer: parser isolated a bad file",
			zap.String("file", relPath), zap.String("reason", res.Err))
		return quarantineResult(relPath, lang, res.Err), true,
			fmt.Errorf("parser crash isolated on %s: %s", relPath, res.Err)
	case res.Err != "":
		return nil, false, errors.New(res.Err)
	}

	// Clean parse: if the file was quarantined under an older revision
	// and has since been fixed, drop the stale entry.
	q.Forget(relPath)
	result = &parser.ExtractionResult{Nodes: res.Nodes, Edges: res.Edges}
	if res.HasParseErr {
		stampParseErrorCount(result.Nodes, res.ParseErrors)
	}
	return result, false, nil
}

// quarantineResult builds a synthetic single-node result for a file
// that could not be parsed, so the file stays visible in the graph
// with the failure reason attached.
func quarantineResult(relPath, lang, reason string) *parser.ExtractionResult {
	return &parser.ExtractionResult{
		Nodes: []*graph.Node{{
			ID:       relPath,
			Kind:     graph.KindFile,
			Name:     filepath.Base(relPath),
			FilePath: relPath,
			Language: lang,
			Meta: map[string]any{
				"parse_error": reason,
				"quarantined": true,
			},
		}},
	}
}

// stampParseErrorCount stamps a known parse-error count onto a file
// node — the subprocess-path equivalent of stampParseErrors, which
// can't run in the parent because the parse tree never crosses the
// process boundary.
func stampParseErrorCount(nodes []*graph.Node, count int) {
	if count <= 0 {
		return
	}
	for _, n := range nodes {
		if n.Kind != graph.KindFile {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta["parse_errors"] = count
		n.Meta["has_parse_errors"] = true
		return
	}
}

package crashpool

import (
	"encoding/gob"
	"fmt"
	"io"
	"runtime/debug"

	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// RunWorker is the subprocess entry point. It serves extract requests
// decoded from r and writes gob-encoded responses to w until r reaches
// EOF, then returns nil.
//
// It is invoked by the hidden `gortex __parse-worker` subcommand. The
// parent (Pool) owns the lifecycle: a worker is expected to run until
// its stdin is closed or it is killed.
func RunWorker(r io.Reader, w io.Writer) error {
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	return serveWorker(reg, r, w)
}

// serveWorker is the decode/extract/encode loop, factored out of
// RunWorker so tests can drive it with an in-memory registry.
func serveWorker(reg *parser.Registry, r io.Reader, w io.Writer) error {
	dec := gob.NewDecoder(r)
	enc := gob.NewEncoder(w)
	for {
		var req extractRequest
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		resp := serveOne(reg, req)
		if err := enc.Encode(&resp); err != nil {
			return err
		}
	}
}

// serveOne runs one extraction under panic recovery. A Go panic in an
// extractor (a bug, deep recursion, an out-of-range index) is converted
// to an error response so the parent can quarantine the file without
// losing the worker. A hard fault (SIGSEGV in the C grammar, OOM kill)
// is not recoverable here — it kills the process, and the parent
// detects it as a pipe EOF.
func serveOne(reg *parser.Registry, req extractRequest) (resp extractResponse) {
	resp.Seq = req.Seq
	defer func() {
		if rec := recover(); rec != nil {
			resp.Nodes = nil
			resp.Edges = nil
			resp.Err = fmt.Sprintf("extractor panic: %v\n%s", rec, debug.Stack())
			resp.Panicked = true
		}
	}()

	ext, ok := reg.GetByLanguage(req.Language)
	if !ok || ext == nil {
		resp.Err = "crashpool: no extractor for language " + req.Language
		return resp
	}
	result, err := ext.Extract(req.RelPath, req.Content)
	if err != nil {
		resp.Err = err.Error()
		return resp
	}
	if result != nil {
		resp.Nodes = result.Nodes
		resp.Edges = result.Edges
		if result.Tree != nil {
			if result.Tree.HasParseErrors() {
				resp.HasParseErr = true
				resp.ParseErrors = result.Tree.CountParseErrors()
			}
			result.Tree.Release()
		}
	}
	return resp
}

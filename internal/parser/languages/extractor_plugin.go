package languages

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// defaultExtractorPluginTimeout bounds a plugin subprocess when the
// spec doesn't set TimeoutMs. Generous enough for a real extractor pass
// over a single file, small enough that a hung plugin never stalls the
// whole index.
const defaultExtractorPluginTimeout = 5000 * time.Millisecond

// pluginNode mirrors one entry of the plugin's JSON `nodes` array. The
// field names are the documented wire shape; meta is free-form.
type pluginNode struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	FilePath  string         `json:"file_path"`
	StartLine int            `json:"start_line"`
	EndLine   int            `json:"end_line"`
	Language  string         `json:"language"`
	Meta      map[string]any `json:"meta"`
}

// pluginEdge mirrors one entry of the plugin's JSON `edges` array.
type pluginEdge struct {
	From     string         `json:"from"`
	To       string         `json:"to"`
	Kind     string         `json:"kind"`
	FilePath string         `json:"file_path"`
	Line     int            `json:"line"`
	Meta     map[string]any `json:"meta"`
}

// pluginDocument is the full JSON document a plugin writes to stdout.
type pluginDocument struct {
	Nodes []pluginNode `json:"nodes"`
	Edges []pluginEdge `json:"edges"`
}

// SubprocessExtractor is a parser.Extractor backed by an external
// command — the "register a custom extractor pass without writing Go"
// entry point. Extract pipes the file content to the command (with
// GORTEX_FILE_PATH in the env), reads a JSON node/edge document from
// stdout, and folds it into the graph. Any failure (non-zero exit, bad
// JSON, timeout) degrades to just the file node so a misbehaving plugin
// never fails the whole index.
type SubprocessExtractor struct {
	language string
	exts     []string
	command  string
	args     []string
	timeout  time.Duration
	log      *zap.Logger
	dropOnce sync.Once
}

// NewSubprocessExtractor builds a SubprocessExtractor from a spec. The
// extensions are normalised (leading dot enforced); a zero TimeoutMs
// uses defaultExtractorPluginTimeout.
func NewSubprocessExtractor(spec config.ExtractorPluginSpec, log *zap.Logger) *SubprocessExtractor {
	if log == nil {
		log = zap.NewNop()
	}
	timeout := defaultExtractorPluginTimeout
	if spec.TimeoutMs > 0 {
		timeout = time.Duration(spec.TimeoutMs) * time.Millisecond
	}
	return &SubprocessExtractor{
		language: strings.TrimSpace(spec.Language),
		exts:     normalizeGrammarExtensions(spec.Extensions),
		command:  strings.TrimSpace(spec.Command),
		args:     append([]string(nil), spec.Args...),
		timeout:  timeout,
		log:      log,
	}
}

func (e *SubprocessExtractor) Language() string     { return e.language }
func (e *SubprocessExtractor) Extensions() []string { return e.exts }

// fileNode builds the always-emitted KindFile node for a path. Its ID
// is the file path, matching every built-in extractor's convention so
// EdgeDefines from plugins resolve against the same file node.
func (e *SubprocessExtractor) fileNode(filePath string, src []byte) *graph.Node {
	return &graph.Node{
		ID:        filePath,
		Kind:      graph.KindFile,
		Name:      filePath,
		FilePath:  filePath,
		StartLine: 1,
		EndLine:   bytes.Count(src, []byte("\n")) + 1,
		Language:  e.language,
	}
}

// Extract runs the plugin command over src and returns the file node
// plus every valid node/edge the plugin emitted. Any error path returns
// just the file node and a nil error — the index is never failed by a
// plugin.
func (e *SubprocessExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	fileNode := e.fileNode(filePath, src)
	result.Nodes = append(result.Nodes, fileNode)

	if e.command == "" {
		return result, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()

	// command/args are operator-declared config, not user-derived input.
	cmd := exec.CommandContext(ctx, e.command, e.args...) //nolint:gosec
	cmd.Stdin = bytes.NewReader(src)
	cmd.Env = append(os.Environ(), "GORTEX_FILE_PATH="+filePath)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		e.log.Warn("extractor plugin: command failed — degraded to file node only",
			zap.String("language", e.language), zap.String("file", filePath),
			zap.String("stderr", strings.TrimSpace(errBuf.String())), zap.Error(err))
		return result, nil
	}

	var doc pluginDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		e.log.Warn("extractor plugin: invalid JSON output — degraded to file node only",
			zap.String("language", e.language), zap.String("file", filePath), zap.Error(err))
		return result, nil
	}

	for _, pn := range doc.Nodes {
		node := e.toNode(filePath, pn)
		if node == nil {
			continue
		}
		result.Nodes = append(result.Nodes, node)
	}
	for _, pe := range doc.Edges {
		edge := e.toEdge(filePath, pe)
		if edge == nil {
			continue
		}
		result.Edges = append(result.Edges, edge)
	}
	return result, nil
}

// toNode converts one plugin node into a graph.Node, applying sensible
// defaults. A node whose Kind is not a valid graph node kind is dropped
// (with a single logged warning per extractor instance) — an invalid
// kind must never poison the graph.
func (e *SubprocessExtractor) toNode(filePath string, pn pluginNode) *graph.Node {
	kind := graph.NodeKind(strings.TrimSpace(pn.Kind))
	if !graph.ValidNodeKind(kind) {
		e.dropOnce.Do(func() {
			e.log.Warn("extractor plugin: dropping node(s) with invalid kind",
				zap.String("language", e.language), zap.String("kind", pn.Kind))
		})
		return nil
	}
	fp := pn.FilePath
	if fp == "" {
		fp = filePath
	}
	lang := pn.Language
	if lang == "" {
		lang = e.language
	}
	id := strings.TrimSpace(pn.ID)
	if id == "" {
		id = fp + "::" + pn.Name
	}
	return &graph.Node{
		ID:        id,
		Kind:      kind,
		Name:      pn.Name,
		FilePath:  fp,
		StartLine: pn.StartLine,
		EndLine:   pn.EndLine,
		Language:  lang,
		Meta:      pn.Meta,
	}
}

// toEdge converts one plugin edge into a graph.Edge. An edge missing
// from/to/kind is dropped; the file path defaults to the source file.
func (e *SubprocessExtractor) toEdge(filePath string, pe pluginEdge) *graph.Edge {
	from := strings.TrimSpace(pe.From)
	to := strings.TrimSpace(pe.To)
	kind := strings.TrimSpace(pe.Kind)
	if from == "" || to == "" || kind == "" {
		return nil
	}
	fp := pe.FilePath
	if fp == "" {
		fp = filePath
	}
	return &graph.Edge{
		From:     from,
		To:       to,
		Kind:     graph.EdgeKind(kind),
		FilePath: fp,
		Line:     pe.Line,
		Meta:     pe.Meta,
	}
}

var _ parser.Extractor = (*SubprocessExtractor)(nil)

// RegisterExtractorPlugins registers every configured subprocess
// extractor plugin, mirroring RegisterCustomGrammars: a spec whose
// language or any extension collides with a built-in (or already
// registered) extractor is skipped — built-ins win — and a spec that
// fails to validate is skipped, each with a logged warning so a typo
// never aborts startup.
func RegisterExtractorPlugins(reg *parser.Registry, specs []config.ExtractorPluginSpec, log *zap.Logger) {
	if reg == nil {
		return
	}
	if log == nil {
		log = zap.NewNop()
	}
	for _, spec := range specs {
		lang := strings.TrimSpace(spec.Language)
		exts := normalizeGrammarExtensions(spec.Extensions)
		if lang == "" || strings.TrimSpace(spec.Command) == "" || len(exts) == 0 {
			log.Warn("extractor plugin: skipped — language, command and extensions are all required",
				zap.String("language", spec.Language), zap.String("command", spec.Command))
			continue
		}
		if _, exists := reg.GetByLanguage(lang); exists {
			log.Warn("extractor plugin: skipped — language already registered by a built-in extractor",
				zap.String("language", lang))
			continue
		}
		if conflict := firstClaimedExtension(reg, exts); conflict != "" {
			log.Warn("extractor plugin: skipped — extension already claimed by a built-in extractor",
				zap.String("language", lang), zap.String("extension", conflict))
			continue
		}
		spec.Extensions = exts
		reg.Register(NewSubprocessExtractor(spec, log))
		log.Info("extractor plugin registered",
			zap.String("language", lang), zap.Strings("extensions", exts),
			zap.String("command", spec.Command))
	}
}

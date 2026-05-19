package indexer

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
)

// defaultTransformTimeout bounds a transform subprocess when the rule
// does not set one.
const defaultTransformTimeout = 30 * time.Second

// contentTransform rewrites a file's raw bytes before extraction.
type contentTransform interface {
	name() string
	matches(path string) bool
	apply(path string, src []byte) ([]byte, error)
	// asLanguage returns a non-empty language when this transform
	// re-types matching files to a language their extension does not
	// natively map to.
	asLanguage() string
}

// transformPipeline applies an ordered list of content transforms to a
// file's bytes before the parser sees them.
type transformPipeline struct {
	transforms []contentTransform
	logger     *zap.Logger
}

// newTransformPipeline builds the pipeline: the always-on BOM stripper
// followed by every user-declared external-command transform, in
// config order.
func newTransformPipeline(rules []config.TransformRule, logger *zap.Logger) *transformPipeline {
	if logger == nil {
		logger = zap.NewNop()
	}
	p := &transformPipeline{logger: logger}
	p.transforms = append(p.transforms, bomStripTransform{})
	for _, r := range rules {
		if len(r.Command) == 0 {
			logger.Warn("indexer: transform rule has no command; ignored",
				zap.String("name", r.Name))
			continue
		}
		p.transforms = append(p.transforms, newCommandTransform(r))
	}
	return p
}

// run applies every matching transform to src in order. A transform
// that errors is logged and skipped — the bytes from the previous
// stage are kept, so one failing processor never drops a file.
func (p *transformPipeline) run(path string, src []byte) []byte {
	if p == nil {
		return src
	}
	out := src
	for _, t := range p.transforms {
		if !t.matches(path) {
			continue
		}
		res, err := t.apply(path, out)
		if err != nil {
			p.logger.Warn("indexer: content transform failed; keeping untransformed bytes",
				zap.String("transform", t.name()), zap.String("file", path), zap.Error(err))
			continue
		}
		out = res
	}
	return out
}

// languageFor returns the language a transform re-types path to, or ""
// when no transform claims it. Lets a file whose extension is not
// natively indexed (e.g. .pdf) still reach an extractor.
func (p *transformPipeline) languageFor(path string) string {
	if p == nil {
		return ""
	}
	for _, t := range p.transforms {
		if t.asLanguage() != "" && t.matches(path) {
			return t.asLanguage()
		}
	}
	return ""
}

// effectiveLanguage detects a file's language: its native extension
// mapping first, then any transform rule that re-types it.
func (idx *Indexer) effectiveLanguage(path string) (string, bool) {
	if lang, ok := idx.registry.DetectLanguage(path); ok {
		return lang, true
	}
	if lang := idx.transforms.languageFor(path); lang != "" {
		return lang, true
	}
	return "", false
}

// --- built-in: BOM strip -------------------------------------------------

// bomStripTransform removes a leading UTF-8 / UTF-16 byte-order mark. A
// BOM at offset 0 is not whitespace to a tree-sitter grammar and breaks
// the first token (e.g. a Go file's `package` clause), so stripping it
// is always correct — this transform is on for every file.
type bomStripTransform struct{}

func (bomStripTransform) name() string        { return "bom-strip" }
func (bomStripTransform) matches(string) bool { return true }
func (bomStripTransform) asLanguage() string  { return "" }
func (bomStripTransform) apply(_ string, src []byte) ([]byte, error) {
	return stripBOM(src), nil
}

// stripBOM drops a leading UTF-8, UTF-16LE or UTF-16BE byte-order mark.
func stripBOM(src []byte) []byte {
	switch {
	case len(src) >= 3 && src[0] == 0xEF && src[1] == 0xBB && src[2] == 0xBF:
		return src[3:]
	case len(src) >= 2 && src[0] == 0xFF && src[1] == 0xFE:
		return src[2:]
	case len(src) >= 2 && src[0] == 0xFE && src[1] == 0xFF:
		return src[2:]
	default:
		return src
	}
}

// --- user-pluggable: external command ------------------------------------

// commandTransform pipes a file's content through an external program:
// content in on stdin, transformed content out on stdout.
type commandTransform struct {
	rname   string
	exts    map[string]bool
	argv    []string
	asLang  string
	timeout time.Duration
}

func newCommandTransform(r config.TransformRule) *commandTransform {
	exts := make(map[string]bool, len(r.Extensions))
	for _, e := range r.Extensions {
		exts[strings.ToLower(e)] = true
	}
	timeout := defaultTransformTimeout
	if r.TimeoutMillis > 0 {
		timeout = time.Duration(r.TimeoutMillis) * time.Millisecond
	}
	name := r.Name
	if name == "" {
		name = r.Command[0]
	}
	return &commandTransform{
		rname:   name,
		exts:    exts,
		argv:    append([]string(nil), r.Command...),
		asLang:  r.AsLanguage,
		timeout: timeout,
	}
}

func (c *commandTransform) name() string       { return c.rname }
func (c *commandTransform) asLanguage() string { return c.asLang }

func (c *commandTransform) matches(path string) bool {
	if len(c.exts) == 0 {
		return true
	}
	return c.exts[strings.ToLower(filepath.Ext(path))]
}

func (c *commandTransform) apply(_ string, src []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	// argv is operator-declared config, not user-derived input.
	cmd := exec.CommandContext(ctx, c.argv[0], c.argv[1:]...) //nolint:gosec
	cmd.Stdin = bytes.NewReader(src)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if stderr := strings.TrimSpace(errBuf.String()); stderr != "" {
			return nil, fmt.Errorf("%w: %s", err, stderr)
		}
		return nil, err
	}
	return out.Bytes(), nil
}

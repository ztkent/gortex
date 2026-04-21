package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/eval/recall"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

var (
	evalEmbeddersFixture string
	evalEmbeddersIndex   string
	evalEmbeddersFormat  string
	evalEmbeddersOut     string
	evalEmbeddersVariants string
	evalEmbeddersSkipQuality bool
	evalEmbeddersProbeQueries int
)

var evalEmbeddersCmd = &cobra.Command{
	Use:   "embedders",
	Short: "Quality vs speed benchmark across ONNX variants of MiniLM-L6-v2",
	Long: `Benchmarks each ONNX variant of the bundled MiniLM-L6-v2 embedder
and reports model size, embed latency (query + corpus), and retrieval
recall on the fixture. Use this to decide which variant to pin for
your deployment.

Variants (--variants):
  fp32         — onnx/model.onnx (baseline, highest quality)
  o2 / o3 / o4 — ONNX Runtime graph optimizations (bit-identical output)
  qint8_arm64  — INT8 quantized, arm64-tuned (~2–3× faster, ~4× smaller)
  qint8_avx512 — INT8 quantized, AVX-512-tuned
  quint8_avx2  — UINT8 quantized, AVX2-tuned (widest x86 support)

--skip-quality only measures size + embed latency (no re-index, no
fixture run). Useful on CI where you just want a quick size/speed
signal.

Defaults to comparing fp32 vs the arch-matched qint8/quint8 variant
so the table has a direct quality-vs-speed trade.`,
	RunE: runEvalEmbedders,
}

func init() {
	evalEmbeddersCmd.Flags().StringVar(&evalEmbeddersFixture, "fixture", "bench/fixtures/retrieval.yaml", "fixture YAML path")
	evalEmbeddersCmd.Flags().StringVar(&evalEmbeddersIndex, "index", ".", "repo to index for the quality pass")
	evalEmbeddersCmd.Flags().StringVar(&evalEmbeddersFormat, "format", "markdown", "output format: markdown or json")
	evalEmbeddersCmd.Flags().StringVar(&evalEmbeddersOut, "out", "", "output file (default stdout)")
	evalEmbeddersCmd.Flags().StringVar(&evalEmbeddersVariants, "variants", "", "comma-separated variant names (default: fp32 + arch-matched quantized)")
	evalEmbeddersCmd.Flags().BoolVar(&evalEmbeddersSkipQuality, "skip-quality", false, "skip the re-index + fixture pass (size + latency only)")
	evalEmbeddersCmd.Flags().IntVar(&evalEmbeddersProbeQueries, "probe-queries", 64, "number of query embeddings used for the latency probe")
	evalCmd.AddCommand(evalEmbeddersCmd)
}

// embedderResult is one row in the comparison table.
type embedderResult struct {
	Variant          string  `json:"variant"`
	OnnxFile         string  `json:"onnx_file"`
	Dimensions       int     `json:"dimensions"`
	ModelSizeMB      float64 `json:"model_size_mb"`
	InitMs           int64   `json:"init_ms"`
	EmbedP50Micros   int64   `json:"embed_p50_micros"`
	EmbedP95Micros   int64   `json:"embed_p95_micros"`
	IndexMs          int64   `json:"index_ms,omitempty"`
	Recall           map[int]float64 `json:"recall,omitempty"`
	MeanRRank        float64 `json:"mean_reciprocal_rank,omitempty"`
	Notes            string  `json:"notes,omitempty"`
}

type embeddersReport struct {
	Fixture string           `json:"fixture"`
	Cases   int              `json:"cases"`
	Arch    string           `json:"arch"`
	Rows    []embedderResult `json:"rows"`
}

func runEvalEmbedders(_ *cobra.Command, _ []string) error {
	// Resolve which variants to test.
	variants := pickVariants(evalEmbeddersVariants)
	if len(variants) == 0 {
		return fmt.Errorf("no variants selected")
	}

	// Load fixture (only used when --skip-quality is off).
	var fixture recall.Fixture
	fixtureBytes, err := os.ReadFile(evalEmbeddersFixture)
	if err != nil {
		return fmt.Errorf("reading fixture: %w", err)
	}
	if err := yaml.Unmarshal(fixtureBytes, &fixture); err != nil {
		return fmt.Errorf("parsing fixture: %w", err)
	}
	if fixture.Name == "" {
		fixture.Name = filepath.Base(evalEmbeddersFixture)
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	absIndex, err := filepath.Abs(evalEmbeddersIndex)
	if err != nil {
		return fmt.Errorf("resolving index path: %w", err)
	}

	report := embeddersReport{
		Fixture: fixture.Name,
		Cases:   len(fixture.Cases),
		Arch:    runtime.GOOS + "/" + runtime.GOARCH,
	}

	// Gather probe texts (first N fixture queries) once so latency is
	// measured over identical input across variants.
	probeTexts := make([]string, 0, evalEmbeddersProbeQueries)
	for _, c := range fixture.Cases {
		if len(probeTexts) >= evalEmbeddersProbeQueries {
			break
		}
		probeTexts = append(probeTexts, c.Query)
	}

	for _, name := range variants {
		row, err := benchVariant(name, probeTexts, fixture, cfg, absIndex, evalEmbeddersSkipQuality)
		if err != nil {
			row.Notes = err.Error()
		}
		report.Rows = append(report.Rows, row)
	}

	var out []byte
	switch evalEmbeddersFormat {
	case "markdown", "md":
		out = []byte(renderEmbeddersMarkdown(report, evalEmbeddersSkipQuality))
	case "json":
		out, err = json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding JSON: %w", err)
		}
		out = append(out, '\n')
	default:
		return fmt.Errorf("unknown format: %s", evalEmbeddersFormat)
	}

	if evalEmbeddersOut == "" {
		_, _ = os.Stdout.Write(out)
		return nil
	}
	if err := os.WriteFile(evalEmbeddersOut, out, 0o644); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[gortex eval embedders] wrote %s\n", evalEmbeddersOut)
	return nil
}

// pickVariants resolves the --variants flag. When empty, defaults to
// fp32 + the arch-matched quantized variant: qint8_arm64 on arm64,
// quint8_avx2 on amd64, else just fp32.
func pickVariants(csv string) []string {
	if csv != "" {
		var out []string
		for _, p := range strings.Split(csv, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	switch runtime.GOARCH {
	case "arm64":
		return []string{"fp32", "qint8_arm64"}
	case "amd64":
		return []string{"fp32", "quint8_avx2"}
	default:
		return []string{"fp32"}
	}
}

// benchVariant loads one ONNX variant, measures init + embed latency,
// optionally re-indexes + runs the semantic ranker, and returns one row.
func benchVariant(name string, probeTexts []string, fixture recall.Fixture, cfg *config.Config, absIndex string, skipQuality bool) (embedderResult, error) {
	row := embedderResult{Variant: name}
	spec, ok := embedding.LookupHugotVariant(name)
	if !ok {
		return row, fmt.Errorf("unknown variant %q", name)
	}
	row.OnnxFile = spec.OnnxFile
	row.ModelSizeMB = onnxSizeMB(spec)

	fmt.Fprintf(os.Stderr, "[gortex eval embedders] %s: loading (%s)...\n", name, spec.RepoID)
	initStart := time.Now()
	prov, err := embedding.NewHugotProviderWithVariant(name)
	if err != nil {
		return row, err
	}
	defer func() { _ = prov.Close() }()
	row.InitMs = time.Since(initStart).Milliseconds()
	row.Dimensions = prov.Dimensions()
	if row.ModelSizeMB == 0 {
		// Pre-download size was unavailable; retry after load so the
		// size column reflects the cached file.
		row.ModelSizeMB = onnxSizeMB(spec)
	}

	// Query-embed latency probe.
	lats := make([]int64, 0, len(probeTexts))
	ctx := context.Background()
	for _, q := range probeTexts {
		t0 := time.Now()
		if _, err := prov.Embed(ctx, q); err != nil {
			return row, fmt.Errorf("embed probe: %w", err)
		}
		lats = append(lats, time.Since(t0).Microseconds())
	}
	row.EmbedP50Micros, row.EmbedP95Micros = latPercentiles(lats)

	if skipQuality {
		return row, nil
	}

	// Re-index with this embedder so the vector backend is populated
	// with the variant's own embeddings — anything less is mix-and-match.
	fmt.Fprintf(os.Stderr, "[gortex eval embedders] %s: indexing...\n", name)
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	idx.SetEmbedder(prov)

	idxStart := time.Now()
	if _, err := idx.Index(absIndex); err != nil {
		return row, fmt.Errorf("indexing: %w", err)
	}
	row.IndexMs = time.Since(idxStart).Milliseconds()

	// Pull the hybrid backend's vector side and run semantic-only.
	inner := idx.Search()
	if sw, ok := inner.(*search.Swappable); ok {
		inner = sw.Inner()
	}
	hybrid, _ := inner.(*search.HybridBackend)
	if hybrid == nil || hybrid.VectorIndex() == nil || hybrid.VectorIndex().Count() == 0 {
		row.Notes = "no vector data after indexing"
		return row, nil
	}

	sem := recall.SemanticRanker("semantic", hybrid.VectorIndex(), prov)
	rep := recall.Run(fixture, []recall.Ranker{sem}, tokenCounter())
	if len(rep.Rankers) == 0 {
		return row, nil
	}
	r := rep.Rankers[0]
	row.Recall = r.Recall
	row.MeanRRank = r.MeanRRank
	return row, nil
}

// onnxSizeMB returns the on-disk size of the specific ONNX file for
// the given variant. The Hugot downloader flattens `<subdir>/<file>.onnx`
// to just `<file>.onnx` in the model directory, so we check both
// candidate paths. Returns 0 if the file isn't cached yet (e.g. when
// called before the first Load pulls the model).
func onnxSizeMB(spec embedding.HugotVariant) float64 {
	home, _ := os.UserHomeDir()
	// Mirror Hugot's cache layout: "<org>/<name>" → "<org>_<name>".
	cacheDir := spec.RepoID
	for i, r := range cacheDir {
		if r == '/' {
			cacheDir = cacheDir[:i] + "_" + cacheDir[i+1:]
			break
		}
	}
	modelDir := filepath.Join(home, ".cache", "gortex", "models", cacheDir)
	candidates := []string{
		filepath.Join(modelDir, spec.OnnxFile),
		filepath.Join(modelDir, filepath.Base(spec.OnnxFile)),
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil {
			return float64(info.Size()) / (1024 * 1024)
		}
	}
	return 0
}

// latPercentiles returns p50 and p95 of a micro-latency slice.
func latPercentiles(lats []int64) (int64, int64) {
	if len(lats) == 0 {
		return 0, 0
	}
	sorted := append([]int64(nil), lats...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1] > sorted[j]; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	p := func(pct float64) int64 {
		return sorted[int(float64(len(sorted)-1)*pct)]
	}
	return p(0.50), p(0.95)
}

// renderEmbeddersMarkdown formats one comparison table and a short
// recommendation based on the measured numbers.
func renderEmbeddersMarkdown(report embeddersReport, skipQuality bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Gortex embedder comparison\n\n")
	fmt.Fprintf(&b, "_Fixture: `%s` · arch: `%s` · %d cases_\n\n",
		report.Fixture, report.Arch, report.Cases)

	if skipQuality {
		fmt.Fprintln(&b, "| variant | onnx file | dim | size MB | init ms | p50 µs | p95 µs |")
		fmt.Fprintln(&b, "|---------|-----------|-----|---------|---------|--------|--------|")
		for _, r := range report.Rows {
			fmt.Fprintf(&b, "| %s | `%s` | %d | %.1f | %d | %d | %d |\n",
				r.Variant, r.OnnxFile, r.Dimensions, r.ModelSizeMB,
				r.InitMs, r.EmbedP50Micros, r.EmbedP95Micros)
		}
	} else {
		fmt.Fprintln(&b, "| variant | onnx file | dim | size MB | init ms | p50 µs | p95 µs | index ms | R@1 | R@5 | R@20 | MRR |")
		fmt.Fprintln(&b, "|---------|-----------|-----|---------|---------|--------|--------|----------|-----|-----|------|-----|")
		for _, r := range report.Rows {
			fmt.Fprintf(&b, "| %s | `%s` | %d | %.1f | %d | %d | %d | %d | %s | %s | %s | %.3f |\n",
				r.Variant, r.OnnxFile, r.Dimensions, r.ModelSizeMB,
				r.InitMs, r.EmbedP50Micros, r.EmbedP95Micros, r.IndexMs,
				pctOrDash(r.Recall[1]),
				pctOrDash(r.Recall[5]),
				pctOrDash(r.Recall[20]),
				r.MeanRRank,
			)
		}
	}

	for _, r := range report.Rows {
		if r.Notes != "" {
			fmt.Fprintf(&b, "\n> **%s**: %s\n", r.Variant, r.Notes)
		}
	}

	fmt.Fprintf(&b, "\n## Recommendation\n\n")
	fmt.Fprintf(&b, "%s\n", recommendation(report, skipQuality))
	return b.String()
}

func pctOrDash(v float64) string {
	if v == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", v*100)
}

// recommendation returns a short quality-vs-speed read based on the
// measured rows. Heuristic, not a formal decision function — enough
// to give users a starting point.
func recommendation(report embeddersReport, skipQuality bool) string {
	if len(report.Rows) < 2 {
		return "Only one variant measured — run with `--variants fp32,qint8_arm64` (arm64) or `--variants fp32,quint8_avx2` (amd64) for a trade-off view."
	}
	var base, fast embedderResult
	for _, r := range report.Rows {
		switch r.Variant {
		case "fp32":
			base = r
		case "qint8_arm64", "quint8_avx2":
			fast = r
		}
	}
	if base.Variant == "" || fast.Variant == "" {
		return "Non-standard variant pair; no heuristic recommendation."
	}

	// Ratio helpers; guard against div by zero.
	speedup := func(a, b int64) float64 {
		if b == 0 || a == 0 {
			return 0
		}
		return float64(a) / float64(b)
	}
	qPct := speedup(base.EmbedP50Micros, fast.EmbedP50Micros)
	sizeRatio := 0.0
	if fast.ModelSizeMB > 0 {
		sizeRatio = base.ModelSizeMB / fast.ModelSizeMB
	}

	var out strings.Builder
	fmt.Fprintf(&out, "- **Pick `%s` (fp32)** for CI, correctness tests, and small corpora where indexing time is irrelevant.\n", base.Variant)
	fmt.Fprintf(&out, "- **Pick `%s`** for daemon mode, large repos, and cold-start-sensitive flows. It is **%.1f× faster per query** and **%.1f× smaller on disk**",
		fast.Variant, qPct, sizeRatio)
	if !skipQuality && base.Recall[5] > 0 && fast.Recall[5] > 0 {
		delta := (base.Recall[5] - fast.Recall[5]) * 100
		fmt.Fprintf(&out, ", with a measured **%.1f pp R@5 quality delta** on this fixture.\n", delta)
	} else {
		fmt.Fprintf(&out, ". Quality delta not measured in this run (pass without `--skip-quality`).\n")
	}
	fmt.Fprintf(&out, "- The difference is **lossy quantization, not optimization**: `_O2`/`_O3`/`_O4` variants would be bit-identical to fp32, just faster. Use them if you want the fp32 quality at O3 speed.\n")
	return out.String()
}

//go:build llama

package svc

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/llm"
)

// TestE2E_AssistAgainstRealModel exercises ExpandQuery and
// RerankSymbols against the model configured at GORTEX_LLM_MODEL (or
// ~/models/qwen2.5-coder-3b-instruct-q4_k_m.gguf if unset). Skipped
// when the file isn't present, so the test is safe to commit and is
// only opted into when a developer has a model on disk.
//
// What it asserts:
//   - ExpandQuery on an NL query returns >=1 non-empty term, none of
//     which is the original query verbatim.
//   - RerankSymbols echoes back a permutation of the candidate IDs.
//   - The dedicated assist context survives back-to-back calls (no
//     KV bleed) and the cache hits the second time.
//
// What it does NOT assert: specific term content or ordering — those
// depend on the model.
func TestE2E_AssistAgainstRealModel(t *testing.T) {
	modelPath := resolveModelPath(t)
	if modelPath == "" {
		t.Skip("no model configured (set GORTEX_LLM_MODEL or place qwen2.5-coder-3b-instruct-q4_k_m.gguf in ~/models)")
	}

	cfg := llm.Config{
		Provider: "local",
		MaxSteps: 16,
		Local: llm.LocalConfig{
			Model:    modelPath,
			Template: "chatml",
			Ctx:      4096,
		},
	}.ApplyDefaults()

	svcInst := NewService(cfg, llm.MockBackend{})
	t.Cleanup(func() { _ = svcInst.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	t.Run("ExpandQuery", func(t *testing.T) {
		query := "where do we hash passwords"
		t0 := time.Now()
		got, err := svcInst.ExpandQuery(ctx, query)
		t.Logf("ExpandQuery %q → %v (%v)", query, got, time.Since(t0))
		if err != nil {
			t.Fatalf("ExpandQuery failed: %v", err)
		}
		if got == nil {
			t.Fatal("got nil result")
		}
		if len(got.Terms) == 0 {
			t.Fatal("model returned 0 terms — prompt may need tuning")
		}
		for _, term := range got.Terms {
			if strings.EqualFold(strings.TrimSpace(term), query) {
				t.Errorf("term echoes original query: %q", term)
			}
			if strings.TrimSpace(term) == "" {
				t.Error("empty term in output")
			}
			if expansionStoplist[strings.ToLower(strings.TrimSpace(term))] {
				t.Errorf("stoplisted generic noun leaked through filter: %q", term)
			}
			if len(strings.TrimSpace(term)) < minExpansionTermLen {
				t.Errorf("sub-min-length term leaked through filter: %q", term)
			}
		}
		if len(got.Terms) > maxExpansionTerms {
			t.Errorf("expansion exceeded cap: %d > %d", len(got.Terms), maxExpansionTerms)
		}

		// Second call must hit the cache.
		t1 := time.Now()
		got2, err := svcInst.ExpandQuery(ctx, query)
		if err != nil {
			t.Fatalf("second ExpandQuery failed: %v", err)
		}
		if !got2.Cached {
			t.Error("expected Cached=true on second call")
		}
		t.Logf("ExpandQuery cached lookup: %v", time.Since(t1))
	})

	t.Run("RerankSymbols", func(t *testing.T) {
		query := "validate user authentication token"
		cands := []llm.RerankCandidate{
			{ID: "pkg/auth.parseJWT", Name: "parseJWT", Signature: "func parseJWT(s string) (*Claims, error)"},
			{ID: "pkg/auth.ValidateToken", Name: "ValidateToken", Signature: "func ValidateToken(tok string) error"},
			{ID: "pkg/auth.AuthMiddleware", Name: "AuthMiddleware", Signature: "func AuthMiddleware(h http.Handler) http.Handler"},
			{ID: "pkg/user.hashPassword", Name: "hashPassword", Signature: "func hashPassword(p string) []byte"},
			{ID: "pkg/user.NewUser", Name: "NewUser", Signature: "func NewUser(name string) *User"},
		}
		t0 := time.Now()
		got, err := svcInst.RerankSymbols(ctx, query, cands)
		t.Logf("RerankSymbols %q → %v (%v)", query, got.Order, time.Since(t0))
		if err != nil {
			t.Fatalf("RerankSymbols failed: %v", err)
		}
		if got == nil {
			t.Fatal("got nil result")
		}
		// Output must be a permutation of input IDs.
		assertPermutation(t, got.Order, cands)
	})

	// The verify scenarios are intentionally soft: they LOG kept ids
	// rather than fail the test. Verify quality is model-dependent
	// (3B fails the disambiguation cases; we want to compare against
	// 7B without breaking CI on the smaller model). The structural
	// invariant (Keep ⊆ input ids) is asserted via assertSubset.
	t.Run("VerifyRelevance_HashPasswords", func(t *testing.T) {
		query := "where do we hash passwords"
		cands := verifyHashPasswordsScenario()
		t0 := time.Now()
		got, err := svcInst.VerifyRelevance(ctx, query, cands)
		t.Logf("VerifyRelevance %q → kept=%v dropped=%d (%v)", query, got.Keep, len(cands)-len(got.Keep), time.Since(t0))
		if err != nil {
			t.Fatalf("VerifyRelevance failed: %v", err)
		}
		assertVerifySubset(t, got.Keep, cands)

		// Soft scoring — we LOG these rather than fail.
		expectKept := "synthetic.pkg.user.HashPassword"
		expectDropped := "real.hashDiagnostics"
		kept := containsID(got.Keep, expectKept)
		dropped := !containsID(got.Keep, expectDropped)
		t.Logf("VERDICT(hash-passwords): kept HashPassword=%v, dropped hashDiagnostics=%v", kept, dropped)
	})

	t.Run("VerifyRelevance_BM25", func(t *testing.T) {
		query := "how does the BM25 search rank symbols"
		cands := verifyBM25Scenario()
		t0 := time.Now()
		got, err := svcInst.VerifyRelevance(ctx, query, cands)
		t.Logf("VerifyRelevance %q → kept=%v dropped=%d (%v)", query, got.Keep, len(cands)-len(got.Keep), time.Since(t0))
		if err != nil {
			t.Fatalf("VerifyRelevance failed: %v", err)
		}
		assertVerifySubset(t, got.Keep, cands)

		expectKept := "real.NewBM25"
		expectDropped := "synthetic.unrelated.parseTSConfig"
		kept := containsID(got.Keep, expectKept)
		dropped := !containsID(got.Keep, expectDropped)
		t.Logf("VERDICT(BM25): kept NewBM25=%v, dropped parseTSConfig=%v", kept, dropped)
	})
}

// verifyHashPasswordsScenario reproduces the live failure case: the
// real hashDiagnostics (which calls sha256.Sum256 on diagnostic JSON)
// alongside a synthetic genuine password hasher and unrelated
// distractors. A strong-enough model should keep HashPassword and
// drop hashDiagnostics on caller/signature evidence.
func verifyHashPasswordsScenario() []llm.VerifyCandidate {
	return []llm.VerifyCandidate{
		{
			ID:        "real.hashDiagnostics",
			Name:      "hashDiagnostics",
			Signature: "func hashDiagnostics(diags []lsp.Diagnostic) string",
			Body: `func hashDiagnostics(diags []lsp.Diagnostic) string {
	if len(diags) == 0 {
		return "empty"
	}
	b, err := json.Marshal(diags)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}`,
			Callers: []llm.CallerInfo{
				{Name: "diagnosticsBroadcaster.publish", Signature: "func (b *diagnosticsBroadcaster) publish(uri string, diags []lsp.Diagnostic)"},
				{Name: "Server.SetLSPDiagnosticsBroadcasting", Signature: "func (s *Server) SetLSPDiagnosticsBroadcasting()"},
			},
		},
		{
			ID:        "synthetic.pkg.user.HashPassword",
			Name:      "HashPassword",
			Signature: "func HashPassword(plain string) ([]byte, error)",
			Body: `func HashPassword(plain string) ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
}`,
			Callers: []llm.CallerInfo{
				{Name: "UserHandler.RegisterUser", Signature: "func (h *UserHandler) RegisterUser(w http.ResponseWriter, r *http.Request)"},
				{Name: "AdminHandler.SetPassword", Signature: "func (h *AdminHandler) SetPassword(userID int, newPlain string) error"},
			},
		},
		{
			ID:        "synthetic.gitCommitHash",
			Name:      "gitCommitHash",
			Signature: "func gitCommitHash(dir string) string",
			Body: `func gitCommitHash(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}`,
			Callers: []llm.CallerInfo{
				{Name: "indexer.snapshot", Signature: "func (i *Indexer) snapshot() Snapshot"},
			},
		},
		{
			ID:        "synthetic.do_setup",
			Name:      "do_setup",
			Signature: "do_setup()",
			Body: `do_setup() {
	echo "provisioning hetzner instance"
	hcloud server create --type cpx21 --image ubuntu-22.04 --name "$1"
}`,
			Callers: nil,
		},
		{
			ID:        "synthetic.findDoBlock",
			Name:      "findDoBlock",
			Signature: "func (e *ElixirExtractor) findDoBlock(callNode *sitter.Node) *sitter.Node",
			Body: `func (e *ElixirExtractor) findDoBlock(callNode *sitter.Node) *sitter.Node {
	for i := uint32(0); i < callNode.ChildCount(); i++ {
		c := callNode.Child(i)
		if c.Type() == "do_block" {
			return c
		}
	}
	return nil
}`,
			Callers: []llm.CallerInfo{
				{Name: "ElixirExtractor.extractDefs", Signature: "func (e *ElixirExtractor) extractDefs(root *sitter.Node, src []byte)"},
			},
		},
	}
}

// verifyBM25Scenario tests the model's ability to keep the genuine
// BM25 ranking implementation while dropping unrelated parsers /
// fixtures.
func verifyBM25Scenario() []llm.VerifyCandidate {
	return []llm.VerifyCandidate{
		{
			ID:        "real.NewBM25",
			Name:      "NewBM25",
			Signature: "func NewBM25() *BM25Backend",
			Body: `func NewBM25() *BM25Backend {
	return &BM25Backend{
		inverted: make(map[string][]posting),
		bigrams:  make(map[string]map[string]struct{}),
		docs:     make(map[string]doc),
	}
}`,
			Callers: []llm.CallerInfo{
				{Name: "indexer.New", Signature: "func New(g *graph.Graph, ...) *Indexer"},
			},
		},
		{
			ID:        "real.BM25Backend.Search",
			Name:      "Search",
			Signature: "func (b *BM25Backend) Search(query string, limit int) []scored",
			Body: `func (b *BM25Backend) Search(query string, limit int) []scored {
	terms := tokenize(query)
	scores := map[string]float64{}
	for _, t := range terms {
		for _, p := range b.inverted[t] {
			scores[p.id] += b.bm25Score(p, t)
		}
	}
	return topK(scores, limit)
}`,
			Callers: []llm.CallerInfo{
				{Name: "Engine.SearchSymbolsScoped", Signature: "func (e *Engine) SearchSymbolsScoped(q string, limit int, opts QueryOptions) []*graph.Node"},
			},
		},
		{
			ID:        "synthetic.unrelated.parseTSConfig",
			Name:      "parseTSConfig",
			Signature: "func parseTSConfig(path string) (*tsConfig, error)",
			Body: `func parseTSConfig(path string) (*tsConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg tsConfig
	return &cfg, json.Unmarshal(b, &cfg)
}`,
			Callers: []llm.CallerInfo{
				{Name: "TypeScriptExtractor.detect", Signature: "func (e *TypeScriptExtractor) detect(path string) bool"},
			},
		},
		{
			ID:        "synthetic.unrelated.do_eval",
			Name:      "do_eval",
			Signature: "do_eval()",
			Body: `do_eval() {
	ssh root@"$IP" "cd /opt/gortex && ./run-bench.sh"
}`,
			Callers: nil,
		},
	}
}

func assertVerifySubset(t *testing.T, kept []string, cands []llm.VerifyCandidate) {
	t.Helper()
	valid := make(map[string]bool, len(cands))
	for _, c := range cands {
		valid[c.ID] = true
	}
	for _, id := range kept {
		if !valid[id] {
			t.Errorf("verify emitted unknown id: %q", id)
		}
	}
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// resolveModelPath checks env first, then the documented default
// location used by the user's daemon config.
func resolveModelPath(t *testing.T) string {
	if p := os.Getenv("GORTEX_LLM_MODEL"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	candidate := filepath.Join(home, "models", "qwen2.5-coder-3b-instruct-q4_k_m.gguf")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

func assertPermutation(t *testing.T, got []string, cands []llm.RerankCandidate) {
	t.Helper()
	if len(got) != len(cands) {
		t.Fatalf("length mismatch: got=%d cands=%d", len(got), len(cands))
	}
	wantSet := make(map[string]bool, len(cands))
	for _, c := range cands {
		wantSet[c.ID] = true
	}
	gotSet := make(map[string]bool, len(got))
	for _, id := range got {
		if !wantSet[id] {
			t.Errorf("rerank emitted unknown id: %q", id)
		}
		if gotSet[id] {
			t.Errorf("rerank emitted duplicate id: %q", id)
		}
		gotSet[id] = true
	}
	if len(gotSet) != len(wantSet) {
		gotKeys := keysOf(gotSet)
		wantKeys := keysOf(wantSet)
		sort.Strings(gotKeys)
		sort.Strings(wantKeys)
		t.Fatalf("missing ids:\n  got:  %v\n  want: %v", gotKeys, wantKeys)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

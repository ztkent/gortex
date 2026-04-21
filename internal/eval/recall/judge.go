package recall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Judge rescues recall misses by asking a cheap LLM whether any symbol
// in the ranker's top-K plausibly answers the query, modelling CQS-
// style dual-judge evaluation. Verdicts are cached on disk per
// (query, top-K set) so re-runs don't re-pay.
type Judge struct {
	Model      string // Anthropic model ID, e.g. "claude-haiku-4-5"
	APIKey     string // from ANTHROPIC_API_KEY
	CachePath  string // file path for verdict cache
	HTTPClient *http.Client

	cacheOnce sync.Once
	cache     map[string]bool
	cacheMu   sync.Mutex
}

// NewJudge constructs a Judge. Returns nil if api key is missing —
// caller treats nil as "judging disabled."
func NewJudge(model string) *Judge {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" || model == "" {
		return nil
	}
	cacheDir, _ := os.UserCacheDir()
	cachePath := filepath.Join(cacheDir, "gortex", "eval_judge_cache.json")
	return &Judge{
		Model:      model,
		APIKey:     key,
		CachePath:  cachePath,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Verdict asks whether any ID in top plausibly answers query. Returns
// true/false plus the string verdict. Cached by (model, query, sorted
// top list).
func (j *Judge) Verdict(query string, top []string) (bool, error) {
	j.loadCache()
	key := verdictKey(j.Model, query, top)

	j.cacheMu.Lock()
	if v, ok := j.cache[key]; ok {
		j.cacheMu.Unlock()
		return v, nil
	}
	j.cacheMu.Unlock()

	verdict, err := j.ask(query, top)
	if err != nil {
		return false, err
	}

	j.cacheMu.Lock()
	j.cache[key] = verdict
	j.cacheMu.Unlock()
	_ = j.saveCache()
	return verdict, nil
}

// ask POSTs to Anthropic /v1/messages with a YES/NO prompt.
func (j *Judge) ask(query string, top []string) (bool, error) {
	if len(top) == 0 {
		return false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are judging whether a code-search result list contains a valid answer to a natural-language query about a codebase.\n\n")
	fmt.Fprintf(&b, "QUERY: %s\n\n", query)
	fmt.Fprintf(&b, "TOP RESULTS (ranked, ID format `relative/path.go::SymbolName`):\n")
	for i, id := range top {
		fmt.Fprintf(&b, "%d. %s\n", i+1, id)
	}
	fmt.Fprintf(&b, "\nIf ANY listed symbol is a reasonable answer to the query — i.e. a programmer asking this query would accept it as the right thing to look at — reply with the single word YES. Otherwise reply with the single word NO. No preamble, no punctuation.\n")

	payload := map[string]any{
		"model":      j.Model,
		"max_tokens": 4,
		"messages": []map[string]any{
			{"role": "user", "content": b.String()},
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("x-api-key", j.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := j.HTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return false, fmt.Errorf("judge http %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return false, err
	}
	var text string
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	text = strings.TrimSpace(strings.ToUpper(text))
	return strings.HasPrefix(text, "YES"), nil
}

func verdictKey(model, query string, top []string) string {
	sorted := append([]string(nil), top...)
	// Order-independent to let RRF / BM25 share cache entries when
	// they return the same set in different orders. Comment on the
	// tradeoff: if order materially changes the verdict, flip this to
	// preserve order. For gortex this is benign.
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	// hash.Hash.Write is documented to never return an error, so we
	// feed it through Write directly instead of fmt.Fprintln to keep
	// errcheck quiet without _, _ = noise.
	h := sha256.New()
	h.Write([]byte(model + "\n"))
	h.Write([]byte(query + "\n"))
	for _, id := range sorted {
		h.Write([]byte(id + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// loadCache reads the on-disk cache once per Judge lifetime.
func (j *Judge) loadCache() {
	j.cacheOnce.Do(func() {
		j.cache = make(map[string]bool)
		data, err := os.ReadFile(j.CachePath)
		if err != nil {
			return
		}
		_ = json.Unmarshal(data, &j.cache)
	})
}

// saveCache writes atomically via temp+rename.
func (j *Judge) saveCache() error {
	j.cacheMu.Lock()
	data, err := json.MarshalIndent(j.cache, "", "  ")
	j.cacheMu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(j.CachePath), 0o755); err != nil {
		return err
	}
	tmp := j.CachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, j.CachePath)
}

// ApplyJudge rescues misses in the report: for each RankerResult's
// miss list, asks the judge if any top-K entry is a valid answer;
// when yes, bumps Hits[K]+1 for every k where the top-K was within
// bounds (judge operates on the biggest K only — that's the Miss.Top
// length). Returns (rescued count, judge API errors).
func ApplyJudge(report *Report, judge *Judge) (int, []error) {
	if judge == nil {
		return 0, nil
	}
	maxK := 0
	for _, k := range Ks {
		if k > maxK {
			maxK = k
		}
	}

	var rescued int
	var errs []error
	for i := range report.Rankers {
		r := &report.Rankers[i]
		if r.Skipped != "" {
			continue
		}
		for mi := range r.Misses {
			m := &r.Misses[mi]
			// Judge the biggest-K slice the ranker actually returned.
			top := m.Top
			if len(top) > maxK {
				top = top[:maxK]
			}
			ok, err := judge.Verdict(m.Query, top)
			if err != nil {
				errs = append(errs, fmt.Errorf("judge %s/%s: %w", r.Name, m.CaseID, err))
				continue
			}
			m.JudgedHit = &ok
			if !ok {
				continue
			}
			rescued++
			// A judged hit counts at every k where the top slice was
			// non-empty — we don't know the judged rank, so attribute
			// it to the max-K bucket only. This is the safer read
			// vs CQS's R@1 claims; note it in the methodology section.
			for _, k := range Ks {
				if k >= maxK {
					r.Hits[k]++
				}
			}
		}
		// Recompute recall after rescues.
		if r.Cases > 0 {
			for _, k := range Ks {
				r.Recall[k] = float64(r.Hits[k]) / float64(r.Cases)
			}
		}
	}
	return rescued, errs
}

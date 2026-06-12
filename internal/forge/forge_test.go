package forge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v88/github"
)

// newTestClient builds a *ghClient whose go-github client points at the
// given test-server base URL. It exercises the real go-github transport;
// only the base URL (and token + slug) are injected.
func newTestClient(t *testing.T, baseURL string) *ghClient {
	t.Helper()
	base := strings.TrimSuffix(baseURL, "/") + "/"
	gh, err := github.NewClient(github.WithAuthToken("test-token"), github.WithURLs(&base, &base))
	if err != nil {
		t.Fatalf("building github client: %v", err)
	}
	return &ghClient{gh: gh, owner: "octo", repo: "gortex", timeout: 5 * time.Second}
}

// withTestSeam swaps newClient so the free functions resolve to c, then
// restores it. It returns a cleanup the caller defers.
func withTestSeam(t *testing.T, c *ghClient) {
	t.Helper()
	prev := newClient
	newClient = func(ctx context.Context, repoDir string) (*ghClient, error) { return c, nil }
	t.Cleanup(func() { newClient = prev })
}

// fixtureServer routes the recorded GitHub REST endpoints used by forge.
func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/octo/gortex/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(prListJSON))
	})
	mux.HandleFunc("/repos/octo/gortex/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(prGetJSON))
	})
	mux.HandleFunc("/repos/octo/gortex/pulls/7/files", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(prFilesJSON))
	})
	mux.HandleFunc("/repos/octo/gortex/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			// CreateReview
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":1,"state":"COMMENTED"}`))
			return
		}
		_, _ = w.Write([]byte(prReviewsJSON))
	})
	mux.HandleFunc("/repos/octo/gortex/commits/headsha/check-runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(checkRunsJSON))
	})
	mux.HandleFunc("/repos/octo/gortex/commits/headsha/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(combinedStatusJSON))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestListPRs_FilesEmpty(t *testing.T) {
	srv := fixtureServer(t)
	c := newTestClient(t, srv.URL)
	withTestSeam(t, c)

	prs, err := ListPRs(context.Background(), ".", ListOpts{State: "open"})
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("got %d PRs, want 1", len(prs))
	}
	p := prs[0]
	if p.Number != 7 {
		t.Errorf("Number = %d, want 7", p.Number)
	}
	if p.Title != "Add forge" {
		t.Errorf("Title = %q", p.Title)
	}
	if p.Author != "alice" {
		t.Errorf("Author = %q, want alice", p.Author)
	}
	if p.BaseRef != "main" || p.HeadRef != "feature" {
		t.Errorf("Base/Head = %q/%q", p.BaseRef, p.HeadRef)
	}
	if len(p.Files) != 0 {
		t.Errorf("Files must be EMPTY after ListPRs, got %v", p.Files)
	}
	// No aggregates requested → unset.
	if p.ReviewDecision != "" || p.CIRollup != "" {
		t.Errorf("aggregates filled without opt-in: decision=%q ci=%q", p.ReviewDecision, p.CIRollup)
	}
}

func TestListPRs_WithDecisionAndCI(t *testing.T) {
	srv := fixtureServer(t)
	c := newTestClient(t, srv.URL)
	withTestSeam(t, c)

	prs, err := ListPRs(context.Background(), ".", ListOpts{State: "open", WithDecision: true, WithCI: true})
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("got %d PRs, want 1", len(prs))
	}
	p := prs[0]
	if p.ReviewDecision != "CHANGES_REQUESTED" {
		t.Errorf("ReviewDecision = %q, want CHANGES_REQUESTED", p.ReviewDecision)
	}
	if p.CIRollup != "FAILURE" {
		t.Errorf("CIRollup = %q, want FAILURE", p.CIRollup)
	}
}

func TestViewPR_HydratesFilesAndAggregates(t *testing.T) {
	srv := fixtureServer(t)
	c := newTestClient(t, srv.URL)
	withTestSeam(t, c)

	pr, err := ViewPR(context.Background(), ".", 7)
	if err != nil {
		t.Fatalf("ViewPR: %v", err)
	}
	if len(pr.Files) != 2 {
		t.Fatalf("Files = %v, want 2 entries", pr.Files)
	}
	if pr.Files[0] != "internal/forge/forge.go" {
		t.Errorf("Files[0] = %q", pr.Files[0])
	}
	if pr.ReviewDecision != "CHANGES_REQUESTED" {
		t.Errorf("ReviewDecision = %q", pr.ReviewDecision)
	}
	if pr.CIRollup != "FAILURE" {
		t.Errorf("CIRollup = %q", pr.CIRollup)
	}
}

func TestPRFiles(t *testing.T) {
	srv := fixtureServer(t)
	c := newTestClient(t, srv.URL)
	withTestSeam(t, c)

	files, err := PRFiles(context.Background(), ".", 7)
	if err != nil {
		t.Fatalf("PRFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
}

func TestDiffPR_HunksParsed(t *testing.T) {
	srv := fixtureServer(t)
	c := newTestClient(t, srv.URL)
	withTestSeam(t, c)

	diff, err := DiffPR(context.Background(), ".", 7)
	if err != nil {
		t.Fatalf("DiffPR: %v", err)
	}
	if diff.Number != 7 || diff.BaseRef != "main" || diff.HeadRef != "feature" {
		t.Errorf("diff meta = %d %q %q", diff.Number, diff.BaseRef, diff.HeadRef)
	}
	if len(diff.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(diff.Files))
	}
	// First file's patch must parse into at least one hunk anchored to it.
	var hunked *PRFile
	for i := range diff.Files {
		if len(diff.Files[i].Hunks) > 0 {
			hunked = &diff.Files[i]
			break
		}
	}
	if hunked == nil {
		t.Fatalf("no file produced hunks from its patch")
	}
	if hunked.Hunks[0].FilePath != hunked.Path {
		t.Errorf("hunk FilePath = %q, want %q", hunked.Hunks[0].FilePath, hunked.Path)
	}
	if hunked.Hunks[0].StartLine == 0 {
		t.Errorf("hunk StartLine not parsed: %+v", hunked.Hunks[0])
	}
	if diff.Raw == "" {
		t.Errorf("Raw diff is empty")
	}
}

func TestPostReviewComments(t *testing.T) {
	srv := fixtureServer(t)
	c := newTestClient(t, srv.URL)
	withTestSeam(t, c)

	err := PostReviewComments(context.Background(), ".", 7, []ReviewComment{
		{Path: "internal/forge/forge.go", Line: 12, Body: "nit"},
		{Path: "internal/forge/ghrest.go", StartLine: 4, Line: 9, Side: "RIGHT", Body: "range"},
	})
	if err != nil {
		t.Fatalf("PostReviewComments: %v", err)
	}
}

func TestNewGHClient_NoToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	_, err := newGHClient(context.Background(), ".")
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("err = %v, want ErrNotAuthenticated", err)
	}
}

func TestAvailable(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	if Available(context.Background()) {
		t.Errorf("Available = true with no token")
	}
	t.Setenv("GITHUB_TOKEN", "tok")
	if !Available(context.Background()) {
		t.Errorf("Available = false with GITHUB_TOKEN set")
	}
}

func TestRateLimited(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/gortex/pulls", func(w http.ResponseWriter, r *http.Request) {
		reset := time.Now().Add(42 * time.Second).Unix()
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded","documentation_url":"https://docs.github.com"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	withTestSeam(t, c)

	_, err := ListPRs(context.Background(), ".", ListOpts{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if !strings.Contains(err.Error(), "retry after") {
		t.Errorf("rate-limit error missing Retry-After hint: %v", err)
	}
}

func TestDefaultBranch_Fallback(t *testing.T) {
	// A server with no repos/.../GET handler → API errors → fall back to
	// the local probe (which returns "" outside a repo root).
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/gortex", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	// Should not panic; returns the local-probe fallback ("" here).
	_ = c.DefaultBranch(context.Background())
}

func TestDefaultBranch_FromAPI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/gortex", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"gortex","default_branch":"trunk"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	if got := c.DefaultBranch(context.Background()); got != "trunk" {
		t.Errorf("DefaultBranch = %q, want trunk", got)
	}
}

func TestOwnerRepoFrom(t *testing.T) {
	tests := []struct {
		in, owner, repo string
		ok              bool
	}{
		{"github.com/octo/gortex", "octo", "gortex", true},
		{"github.com/octo/gortex.git", "octo", "gortex", true},
		{"octo/gortex", "octo", "gortex", true},
		{"gortex", "", "", false},
		{"", "", "", false},
		{"ghe.example.com/org/team/repo", "team", "repo", true},
	}
	for _, tt := range tests {
		o, r, ok := ownerRepoFrom(tt.in)
		if ok != tt.ok || o != tt.owner || r != tt.repo {
			t.Errorf("ownerRepoFrom(%q) = %q,%q,%v; want %q,%q,%v", tt.in, o, r, ok, tt.owner, tt.repo, tt.ok)
		}
	}
}

func TestEnterpriseBase(t *testing.T) {
	t.Setenv("GITHUB_API_URL", "")
	t.Setenv("GH_HOST", "")
	if got := enterpriseBase(); got != "" {
		t.Errorf("enterpriseBase with no env = %q, want empty", got)
	}
	t.Setenv("GITHUB_API_URL", "https://github.com/api/v3")
	if got := enterpriseBase(); got != "" {
		t.Errorf("public github.com must not be enterprise: %q", got)
	}
	t.Setenv("GITHUB_API_URL", "https://ghe.corp.example/api/v3")
	if got := enterpriseBase(); got != "https://ghe.corp.example/api/v3/" {
		t.Errorf("enterpriseBase = %q", got)
	}
	t.Setenv("GITHUB_API_URL", "")
	t.Setenv("GH_HOST", "ghe.corp.example")
	if got := enterpriseBase(); got != "https://ghe.corp.example/api/v3/" {
		t.Errorf("enterpriseBase from GH_HOST = %q", got)
	}
}

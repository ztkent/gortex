package conversationlog

import (
	"testing"
	"time"

	"github.com/zzet/gortex/internal/llm"
)

func sampleRecord(session, file, phase string) Record {
	return Record{
		Session:  session,
		Repo:     "gortex",
		File:     file,
		Phase:    phase,
		Provider: "anthropic",
		Model:    "claude-sonnet",
		Request: []llm.Message{
			{Role: llm.RoleSystem, Content: "you are a reviewer"},
			{Role: llm.RoleUser, Content: "review this"},
		},
		Response:     "looks good",
		InputTokens:  100,
		OutputTokens: 20,
		Estimated:    true,
		ElapsedMs:    42,
	}
}

func TestLogger_Disabled_NoOp(t *testing.T) {
	// A Logger with no directory must record nothing.
	l := New("")
	if l.Enabled() {
		t.Fatal("Logger with empty dir must be disabled")
	}
	l.Record(sampleRecord("s1", "a.go", "main"))

	// A reader over the (empty) dir sees no sessions.
	r := NewReader("")
	sessions, err := r.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("disabled logger recorded %d sessions, want 0", len(sessions))
	}
}

func TestLogger_RecordAndRead(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	if !l.Enabled() {
		t.Fatal("Logger with a dir must be enabled")
	}
	t.Cleanup(func() { _ = l.Close() })

	l.Record(sampleRecord("sess-1", "pkg/a.go", "plan"))
	l.Record(sampleRecord("sess-1", "pkg/b.go", "main"))
	l.Record(sampleRecord("sess-2", "pkg/c.go", "main"))

	r := NewReader(dir)
	sessions, err := r.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}

	// LoadSession returns each recorded step in append order.
	recs, err := r.LoadSession("sess-1")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("sess-1 has %d records, want 2", len(recs))
	}
	if recs[0].File != "pkg/a.go" || recs[1].File != "pkg/b.go" {
		t.Fatalf("records out of order: %q, %q", recs[0].File, recs[1].File)
	}
	if recs[0].InputTokens != 100 || recs[0].OutputTokens != 20 || !recs[0].Estimated {
		t.Fatalf("token usage not round-tripped: %+v", recs[0])
	}
	if len(recs[0].Request) != 2 || recs[0].Request[1].Content != "review this" {
		t.Fatalf("request messages not round-tripped: %+v", recs[0].Request)
	}

	// Summary captures distinct files and phases.
	var s1 *SessionSummary
	for i := range sessions {
		if sessions[i].Session == "sess-1" {
			s1 = &sessions[i]
		}
	}
	if s1 == nil {
		t.Fatal("sess-1 not in session list")
	}
	if s1.Records != 2 {
		t.Fatalf("sess-1 summary records = %d, want 2", s1.Records)
	}
	if len(s1.Files) != 2 {
		t.Fatalf("sess-1 files = %v, want 2", s1.Files)
	}
	if len(s1.Phases) != 2 {
		t.Fatalf("sess-1 phases = %v, want 2", s1.Phases)
	}
	if s1.FirstTS.IsZero() || s1.LastTS.IsZero() {
		t.Fatal("session timestamps not populated")
	}
}

func TestLogger_TSStamped(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	rec := sampleRecord("s", "a.go", "main")
	rec.TS = time.Time{} // unset
	before := time.Now().UTC()
	l.Record(rec)
	_ = l.Close()

	recs, err := NewReader(dir).LoadSession("s")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].TS.Before(before.Add(-time.Second)) {
		t.Fatalf("TS not stamped: %v", recs[0].TS)
	}
}

func TestSanitizeSession_PathTraversal(t *testing.T) {
	// A malicious session id must not escape the directory.
	for _, in := range []string{"../../etc/passwd", "a/b/c", "..", "."} {
		got := sanitizeSession(in)
		if got == in {
			t.Errorf("sanitizeSession(%q) returned the raw id", in)
		}
	}
	if got := sanitizeSession("sess-1"); got != "sess-1" {
		t.Errorf("sanitizeSession(sess-1) = %q, want sess-1", got)
	}
}

func TestReader_MissingSession(t *testing.T) {
	dir := t.TempDir()
	r := NewReader(dir)
	recs, err := r.LoadSession("nope")
	if err != nil {
		t.Fatalf("missing session must not error: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("missing session returned %d records", len(recs))
	}
}

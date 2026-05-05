package progress

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSpinnerDisabledModePrintsPlainText(t *testing.T) {
	var buf bytes.Buffer
	sp := NewSpinner(&buf)
	sp.Disable()

	sp.Start("Indexing repository")
	sp.Report("walking files", 0, 0)
	sp.Report("walking files", 50, 100) // same stage, must not re-emit
	sp.Report("parsing", 0, 0)          // new stage, must emit
	sp.Done()

	out := buf.String()
	wants := []string{
		"Indexing repository",
		"walking files",
		"parsing",
		"✓ Indexing repository", // ✓
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q\n--- got ---\n%s", w, out)
		}
	}
	if got := strings.Count(out, "walking files"); got != 1 {
		t.Errorf("expected stage 'walking files' to print exactly once, got %d", got)
	}
}

func TestSpinnerDisabledFailPrintsErrorLine(t *testing.T) {
	var buf bytes.Buffer
	sp := NewSpinner(&buf)
	sp.Disable()

	sp.Start("Stamping blame")
	sp.Fail(errors.New("boom"))

	out := buf.String()
	if !strings.Contains(out, "✗ Stamping blame") {
		t.Errorf("expected ✗ summary, got: %q", out)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("expected error message in output, got: %q", out)
	}
}

func TestSpinnerDoneIsIdempotent(t *testing.T) {
	var buf bytes.Buffer
	sp := NewSpinner(&buf)
	sp.Disable()

	sp.Start("Indexing")
	sp.Done()
	sp.Done() // must not double-print or panic
	sp.Fail(errors.New("late")) // must be a no-op after Done

	if got := strings.Count(buf.String(), "✓"); got != 1 {
		t.Errorf("expected exactly one ✓, got %d in:\n%s", got, buf.String())
	}
	if strings.Contains(buf.String(), "✗") {
		t.Errorf("expected no ✗ after successful Done, got: %s", buf.String())
	}
}

func TestMultiFansOutAndSkipsNil(t *testing.T) {
	a := &countingReporter{}
	b := &countingReporter{}
	r := Multi(a, nil, b)

	r.Report("walk", 1, 10)
	r.Report("parse", 0, 0)

	if a.calls != 2 || b.calls != 2 {
		t.Errorf("expected 2 calls each, got a=%d b=%d", a.calls, b.calls)
	}
}

func TestMultiCollapsesToSingle(t *testing.T) {
	a := &countingReporter{}
	r := Multi(nil, a, nil)
	if r != a {
		t.Errorf("expected Multi with one non-nil to return that reporter directly")
	}
}

func TestMultiAllNilReturnsNop(t *testing.T) {
	r := Multi(nil, nil)
	if _, ok := r.(Nop); !ok {
		t.Errorf("expected Nop when all inputs nil, got %T", r)
	}
}

type countingReporter struct{ calls int }

func (c *countingReporter) Report(string, int, int) { c.calls++ }

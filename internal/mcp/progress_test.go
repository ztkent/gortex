package mcp

import (
	"context"
	"sync"
	"testing"
)

type fakeSender struct {
	mu    sync.Mutex
	calls []sentNotification
}

type sentNotification struct {
	method string
	params map[string]any
}

func (f *fakeSender) SendNotificationToClient(_ context.Context, method string, params map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Copy params so later mutations by the reporter don't affect assertions.
	copied := make(map[string]any, len(params))
	for k, v := range params {
		copied[k] = v
	}
	f.calls = append(f.calls, sentNotification{method: method, params: copied})
	return nil
}

func TestNewProgressReporter_NilSender_ReturnsNil(t *testing.T) {
	r := newProgressReporter(context.Background(), nil, "tok")
	if r != nil {
		t.Errorf("expected nil reporter when sender is nil, got %T", r)
	}
}

func TestNewProgressReporter_NilToken_ReturnsNil(t *testing.T) {
	r := newProgressReporter(context.Background(), &fakeSender{}, nil)
	if r != nil {
		t.Errorf("expected nil reporter when token is nil, got %T", r)
	}
}

func TestMCPProgressReporter_SendsNotifications(t *testing.T) {
	s := &fakeSender{}
	r := newProgressReporter(context.Background(), s, "t1")
	if r == nil {
		t.Fatal("expected non-nil reporter")
	}

	r.Report("parsing", 5, 100)
	r.Report("parsing", 10, 100)
	r.Report("resolving references", 0, 0)

	if len(s.calls) != 3 {
		t.Fatalf("expected 3 notifications, got %d", len(s.calls))
	}

	for i, c := range s.calls {
		if c.method != "notifications/progress" {
			t.Errorf("call %d: method=%q, want notifications/progress", i, c.method)
		}
		if c.params["progressToken"] != "t1" {
			t.Errorf("call %d: token=%v", i, c.params["progressToken"])
		}
	}
}

func TestMCPProgressReporter_MonotonicProgress(t *testing.T) {
	s := &fakeSender{}
	r := newProgressReporter(context.Background(), s, "t")

	for i := 0; i < 5; i++ {
		r.Report("x", i, 5)
	}

	var last float64
	for i, c := range s.calls {
		p, ok := c.params["progress"].(float64)
		if !ok {
			t.Fatalf("call %d: progress is not float64: %T", i, c.params["progress"])
		}
		if p <= last && i > 0 {
			t.Errorf("call %d: progress %v did not increase from %v", i, p, last)
		}
		last = p
	}
}

func TestMCPProgressReporter_MessageFormatting(t *testing.T) {
	tests := []struct {
		name           string
		stage          string
		current, total int
		wantMessage    string
	}{
		{"with total", "parsing", 42, 100, "parsing (42/100)"},
		{"current only", "parsing", 42, 0, "parsing (42)"},
		{"stage only", "resolving references", 0, 0, "resolving references"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &fakeSender{}
			r := newProgressReporter(context.Background(), s, "t")
			r.Report(tt.stage, tt.current, tt.total)

			if len(s.calls) != 1 {
				t.Fatalf("want 1 call, got %d", len(s.calls))
			}
			if got := s.calls[0].params["message"]; got != tt.wantMessage {
				t.Errorf("message=%q, want %q", got, tt.wantMessage)
			}
		})
	}
}

func TestMCPProgressReporter_TotalOmitted(t *testing.T) {
	s := &fakeSender{}
	r := newProgressReporter(context.Background(), s, "t")
	r.Report("parsing", 5, 100)

	if _, present := s.calls[0].params["total"]; present {
		t.Error("total must be omitted — the reported total changes across stages and would mislead UIs")
	}
}

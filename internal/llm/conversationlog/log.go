// Package conversationlog is an opt-in recorder for the exact LLM
// request/response of each completion, persisted as JSONL per session.
//
// It captures raw prompt and response text plus real token usage, so an
// operator can inspect — locally — exactly what was sent to and returned
// from a provider for a given session, file, and phase. Because it
// records raw model I/O it is OFF by default: a Logger constructed
// without a directory is a cheap no-op, and the directory is only set
// when the operator opts in (GORTEX_CONVERSATION_LOG / config flag).
//
// One file per session lives under <dir>/<session>.jsonl; each line is a
// single Record. Files are size-rotated, mirroring the retrieval
// query-log substrate. All write errors are swallowed — recording must
// never disturb a completion.
package conversationlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/platform"
)

// defaultMaxBytes is the per-session-file rotation threshold.
const defaultMaxBytes int64 = 64 << 20

// Record is one JSONL line: the exact request/response of a single
// completion turn plus its token accounting. Field names are stable
// wire — the WebUI and any offline tooling parse these keys.
type Record struct {
	TS           time.Time     `json:"ts"`
	Session      string        `json:"session,omitempty"`
	Repo         string        `json:"repo,omitempty"`
	File         string        `json:"file,omitempty"`
	Phase        string        `json:"phase,omitempty"`
	Provider     string        `json:"provider,omitempty"`
	Model        string        `json:"model,omitempty"`
	Request      []llm.Message `json:"request"`
	Response     string        `json:"response"`
	InputTokens  int           `json:"input_tokens"`
	OutputTokens int           `json:"output_tokens"`
	Estimated    bool          `json:"estimated"`
	ElapsedMs    int64         `json:"elapsed_ms"`
	Error        string        `json:"error,omitempty"`
}

// Logger appends Records to a per-session JSONL file. A Logger whose
// dir is empty (or whose enabled flag is false) is a no-op: Record
// returns immediately and nothing is written. Safe for concurrent use.
type Logger struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	enabled  bool
	// files maps a session id to its lazily-opened, append-mode handle
	// plus the bytes written so far (for rotation).
	files map[string]*sessionFile
}

type sessionFile struct {
	f       *os.File
	written int64
}

// DirFromEnv resolves the opt-in conversation-log directory from the
// environment. GORTEX_CONVERSATION_LOG either names a directory directly
// or, when set to a truthy flag (1/true/on), selects the default
// <CacheDir>/conversations. Empty / falsey leaves the sink off (the
// privacy-safe default — it records raw LLM I/O). The sink writer and the
// WebUI reader resolve the directory through this one helper so they
// always agree on the location.
func DirFromEnv() string {
	v := strings.TrimSpace(os.Getenv("GORTEX_CONVERSATION_LOG"))
	if v == "" {
		return ""
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return ""
	case "1", "true", "yes", "on":
		return filepath.Join(platform.CacheDir(), "conversations")
	}
	if strings.HasPrefix(v, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, v[2:])
		}
	}
	return v
}

// New constructs a Logger writing under dir. An empty dir yields a
// disabled (no-op) Logger — the opt-in default. Recording is enabled
// only when a non-empty dir is supplied.
func New(dir string) *Logger {
	dir = strings.TrimSpace(dir)
	return &Logger{
		dir:      dir,
		maxBytes: defaultMaxBytes,
		enabled:  dir != "",
		files:    map[string]*sessionFile{},
	}
}

// Enabled reports whether the Logger will record (a directory was set).
func (l *Logger) Enabled() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enabled
}

// Dir returns the configured directory ("" when disabled).
func (l *Logger) Dir() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dir
}

// Record appends one Record to the session's JSONL file. A nil or
// disabled Logger, or a Record with no session id, is a no-op. The TS
// is stamped when zero. All errors are swallowed.
func (l *Logger) Record(rec Record) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.enabled {
		return
	}
	session := sanitizeSession(rec.Session)
	if session == "" {
		session = "default"
	}
	if rec.TS.IsZero() {
		rec.TS = time.Now().UTC()
	}
	line, err := json.Marshal(&rec)
	if err != nil {
		return
	}
	l.appendLocked(session, line)
}

// appendLocked writes one JSONL line to the session file, opening and
// rotating it lazily. Caller holds l.mu.
func (l *Logger) appendLocked(session string, line []byte) {
	sf := l.files[session]
	if sf == nil {
		if err := os.MkdirAll(l.dir, 0o755); err != nil {
			l.enabled = false
			return
		}
		path := l.sessionPath(session)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			l.enabled = false
			return
		}
		sf = &sessionFile{f: f}
		if fi, err := f.Stat(); err == nil {
			sf.written = fi.Size()
		}
		l.files[session] = sf
	}
	if l.maxBytes > 0 && sf.written+int64(len(line))+1 > l.maxBytes {
		l.rotateLocked(session, sf)
	}
	n, err := sf.f.Write(append(line, '\n'))
	if err != nil {
		_ = sf.f.Close()
		delete(l.files, session)
		return
	}
	sf.written += int64(n)
}

// rotateLocked renames the session file to "<path>.1" (one backup) and
// reopens a fresh handle. Caller holds l.mu.
func (l *Logger) rotateLocked(session string, sf *sessionFile) {
	path := l.sessionPath(session)
	if sf.f != nil {
		_ = sf.f.Close()
	}
	_ = os.Rename(path, path+".1")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		delete(l.files, session)
		l.enabled = false
		return
	}
	sf.f = f
	sf.written = 0
}

func (l *Logger) sessionPath(session string) string {
	return filepath.Join(l.dir, session+".jsonl")
}

// Close flushes and closes every open session file. Safe to call
// multiple times and on a disabled Logger.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for s, sf := range l.files {
		if sf.f != nil {
			_ = sf.f.Close()
		}
		delete(l.files, s)
	}
	return nil
}

// --- reader ----------------------------------------------------------

// SessionSummary is the per-session rollup returned by the session
// list: counts, the time span, and the distinct files/phases seen.
type SessionSummary struct {
	Session string    `json:"session"`
	Records int       `json:"records"`
	FirstTS time.Time `json:"first_ts"`
	LastTS  time.Time `json:"last_ts"`
	Files   []string  `json:"files"`
	Phases  []string  `json:"phases"`
}

// Reader loads recorded sessions back from a directory. It is the read
// counterpart to Logger and shares no state — a fresh Reader scans the
// directory on each call so it sees whatever the live Logger has
// flushed.
type Reader struct {
	dir string
}

// NewReader constructs a Reader over the conversation-log directory.
func NewReader(dir string) *Reader { return &Reader{dir: strings.TrimSpace(dir)} }

// errNoDir is returned by reader methods when no directory is set.
var errNoDir = errors.New("conversationlog: no directory configured")

// ListSessions returns a summary per recorded session, sorted by most
// recently active first. A reader with no directory, or a directory
// that does not exist yet, returns an empty slice and no error.
func (r *Reader) ListSessions() ([]SessionSummary, error) {
	if r == nil || r.dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []SessionSummary
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		session := strings.TrimSuffix(name, ".jsonl")
		recs, err := r.LoadSession(session)
		if err != nil {
			continue
		}
		out = append(out, summarize(session, recs))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastTS.After(out[j].LastTS)
	})
	return out, nil
}

// LoadSession reads every Record for a session in append order. A
// session whose file does not exist returns an empty slice and no
// error. Malformed lines are skipped, not fatal.
func (r *Reader) LoadSession(session string) ([]Record, error) {
	if r == nil || r.dir == "" {
		return nil, errNoDir
	}
	session = sanitizeSession(session)
	if session == "" {
		return nil, fmt.Errorf("conversationlog: empty session id")
	}
	path := filepath.Join(r.dir, session+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// summarize folds a session's records into a SessionSummary.
func summarize(session string, recs []Record) SessionSummary {
	s := SessionSummary{Session: session, Records: len(recs)}
	fileSet := map[string]bool{}
	phaseSet := map[string]bool{}
	for i, rec := range recs {
		if i == 0 || rec.TS.Before(s.FirstTS) || s.FirstTS.IsZero() {
			s.FirstTS = rec.TS
		}
		if rec.TS.After(s.LastTS) {
			s.LastTS = rec.TS
		}
		if rec.File != "" {
			fileSet[rec.File] = true
		}
		if rec.Phase != "" {
			phaseSet[rec.Phase] = true
		}
	}
	s.Files = sortedKeys(fileSet)
	s.Phases = sortedKeys(phaseSet)
	return s
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sanitizeSession strips path separators and leading dots so a session
// id can never escape the log directory. An id that reduces to nothing
// after sanitization yields "".
func sanitizeSession(session string) string {
	session = strings.TrimSpace(session)
	if session == "" {
		return ""
	}
	// Drop any directory component an attacker-supplied id might carry,
	// then strip the characters that could still traverse or hide files.
	session = filepath.Base(session)
	session = strings.ReplaceAll(session, string(filepath.Separator), "")
	session = strings.ReplaceAll(session, "/", "")
	session = strings.ReplaceAll(session, "\\", "")
	session = strings.Trim(session, ".")
	return strings.TrimSpace(session)
}

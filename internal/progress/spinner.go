package progress

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// Cozy palette — gortex mark.
var (
	colPerim  = lipgloss.Color("#F0F0F0")
	colInner  = lipgloss.Color("#46464E")
	colAccent = lipgloss.Color("#5FD67A")
	colFg     = lipgloss.Color("#F4F3EF")
	colFgDim  = lipgloss.Color("#969699")
	colErr    = lipgloss.Color("#F06E64")

	stylePerim  = lipgloss.NewStyle().Foreground(colPerim)
	styleInner  = lipgloss.NewStyle().Foreground(colInner)
	styleAccent = lipgloss.NewStyle().Foreground(colAccent)
	styleLabel  = lipgloss.NewStyle().Bold(true).Foreground(colFg)
	styleSub    = lipgloss.NewStyle().Foreground(colFgDim)
	styleOK     = lipgloss.NewStyle().Foreground(colAccent)
	styleX      = lipgloss.NewStyle().Foreground(colErr)
)

// 12 perimeter cells, clockwise from top-left edge.
var spinnerPerimeter = [12][2]int{
	{0, 1}, {0, 2}, {0, 3},
	{1, 4}, {2, 4}, {3, 4},
	{4, 3}, {4, 2}, {4, 1},
	{3, 0}, {2, 0}, {1, 0},
}

// Inner cells promoted to "perimeter" style — the asymmetric horizontal bridge
// in the middle row that's part of the gortex mark.
var spinnerInnerAccents = map[[2]int]bool{
	{2, 2}: true,
	{2, 3}: true,
}

const (
	cellEmpty = iota
	cellPerim
	cellInner
)

func spinnerCellAt(r, c int) int {
	if (r == 0 || r == 4) && (c == 0 || c == 4) {
		return cellEmpty
	}
	if r == 0 || r == 4 || c == 0 || c == 4 {
		return cellPerim
	}
	if spinnerInnerAccents[[2]int{r, c}] {
		return cellPerim
	}
	return cellInner
}

// meshFrame renders the 5-line mesh with the active perimeter node at
// position activeIdx. Used both directly (MeshFrame) and as one of the 12
// frames the bubbles spinner cycles through.
func meshFrame(activeIdx int) string {
	active := spinnerPerimeter[activeIdx%len(spinnerPerimeter)]
	var lines [5]string
	for r := range 5 {
		var row strings.Builder
		for c := range 5 {
			switch spinnerCellAt(r, c) {
			case cellEmpty:
				row.WriteString(" ")
			case cellPerim:
				if r == active[0] && c == active[1] {
					row.WriteString(styleAccent.Render("●"))
				} else {
					row.WriteString(stylePerim.Render("●"))
				}
			case cellInner:
				row.WriteString(styleInner.Render("·"))
			}
			if c < 4 {
				row.WriteString(" ")
			}
		}
		lines[r] = row.String()
	}
	return strings.Join(lines[:], "\n")
}

func meshFrames() []string {
	out := make([]string, len(spinnerPerimeter))
	for i := range spinnerPerimeter {
		out[i] = meshFrame(i)
	}
	return out
}

// MeshFrame returns one frame of the gortex mesh logo with the active node at
// perimeter[tick%12]. label is shown bold next to the mesh and sub (dim)
// underneath. Used by watch loops or custom views that want the cozy block
// without owning a Spinner.
func MeshFrame(tick int, label, sub string) string {
	mesh := meshFrame(tick)
	if label == "" && sub == "" {
		return mesh + "\n"
	}
	right := lipgloss.JoinVertical(
		lipgloss.Left,
		"",
		styleLabel.Render(label),
		"",
		styleSub.Render(sub),
		"",
	)
	return lipgloss.JoinHorizontal(lipgloss.Top, mesh, "    ", right) + "\n"
}

// IsTTY reports whether w is backed by a terminal file descriptor.
func IsTTY(w io.Writer) bool {
	f, ok := w.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

func envDisablesColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return true
	}
	if t := os.Getenv("TERM"); t == "dumb" {
		return true
	}
	return false
}

// ---- bubbletea model ----------------------------------------------------

type meshState int

const (
	meshAnimating meshState = iota
	meshDone
	meshFailed
)

type meshSetMsg struct{ label, sub string }
type meshFinishMsg struct {
	state meshState
	err   error
}

type meshModel struct {
	sp    spinner.Model
	label string
	sub   string
	state meshState
	err   error
}

func newMeshModel(label string) meshModel {
	s := spinner.New()
	s.Spinner = spinner.Spinner{
		Frames: meshFrames(),
		FPS:    80 * time.Millisecond,
	}
	return meshModel{sp: s, label: label}
}

func (m meshModel) Init() tea.Cmd { return m.sp.Tick }

func (m meshModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd
	case meshSetMsg:
		if msg.label != "" {
			m.label = msg.label
		}
		m.sub = msg.sub
		return m, nil
	case meshFinishMsg:
		m.state = msg.state
		m.err = msg.err
		return m, tea.Quit
	}
	return m, nil
}

func (m meshModel) View() string {
	switch m.state {
	case meshDone:
		return fmt.Sprintf("  %s  %s   %s\n",
			styleOK.Render("✓"),
			styleLabel.Render(m.label),
			styleSub.Render(m.sub),
		)
	case meshFailed:
		sub := m.sub
		if m.err != nil {
			sub = m.err.Error()
		}
		return fmt.Sprintf("  %s  %s   %s\n",
			styleX.Render("✗"),
			styleLabel.Render(m.label),
			styleSub.Render(sub),
		)
	default:
		right := lipgloss.JoinVertical(
			lipgloss.Left,
			"",
			styleLabel.Render(m.label),
			"",
			styleSub.Render(m.sub),
			"",
		)
		return lipgloss.JoinHorizontal(lipgloss.Top, m.sp.View(), "    ", right)
	}
}

// ---- public Spinner API -------------------------------------------------

// Spinner is a TTY-aware animated Reporter backed by bubbletea/bubbles.
// When the writer isn't a TTY (or Disable is called), it degrades to plain
// text: a label line on Start, stage transitions on Report, and a one-line
// summary on Done/Fail. Implements Reporter so it can be installed via
// WithReporter.
type Spinner struct {
	w       io.Writer
	enabled bool

	mu        sync.Mutex
	label     string
	sub       string
	lastStage string

	program *tea.Program
	doneCh  chan struct{}

	started atomic.Bool
	stopped atomic.Bool
}

// NewSpinner constructs a Spinner bound to w. When w isn't a TTY (or NO_COLOR
// / TERM=dumb is set), the spinner is created in disabled / plain-text mode.
func NewSpinner(w io.Writer) *Spinner {
	return &Spinner{
		w:       w,
		enabled: IsTTY(w) && !envDisablesColor(),
	}
}

// Disable forces the spinner into plain-text mode.
func (s *Spinner) Disable() { s.enabled = false }

// Enabled reports whether the spinner is animating.
func (s *Spinner) Enabled() bool { return s.enabled }

// Start begins animating with the given label.
func (s *Spinner) Start(label string) {
	s.mu.Lock()
	s.label = label
	s.mu.Unlock()
	if !s.enabled {
		_, _ = fmt.Fprintf(s.w, "  %s\n", label)
		return
	}
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	m := newMeshModel(label)
	// Pass an empty input so tea never reads from stdin and never puts the
	// terminal into raw mode. We don't need keyboard events; the parent
	// shell keeps cooked-mode (LF→CRLF) translation active so any
	// concurrent writes to the real stderr still wrap to column 0.
	s.program = tea.NewProgram(m,
		tea.WithOutput(s.w),
		tea.WithInput(bytes.NewReader(nil)),
		tea.WithoutSignalHandler(),
	)
	s.doneCh = make(chan struct{})
	go func() {
		defer close(s.doneCh)
		_, _ = s.program.Run()
	}()
}

// Set updates the label and sub-status mid-animation. Either may be empty to
// leave the existing value.
func (s *Spinner) Set(label, sub string) {
	s.mu.Lock()
	if label != "" {
		s.label = label
	}
	s.sub = sub
	s.mu.Unlock()
	if s.program != nil {
		s.program.Send(meshSetMsg{label: label, sub: sub})
	}
}

// Report implements Reporter. Each tick from a long-running pass updates the
// sub-status; in plain-text mode, only stage transitions are echoed.
func (s *Spinner) Report(stage string, current, total int) {
	if stage == "" {
		return
	}
	var sub string
	switch {
	case total > 0:
		sub = fmt.Sprintf("%s · %d / %d", stage, current, total)
	case current > 0:
		sub = fmt.Sprintf("%s · %d", stage, current)
	default:
		sub = stage
	}
	if !s.enabled {
		s.mu.Lock()
		newStage := stage != s.lastStage
		s.lastStage = stage
		s.mu.Unlock()
		if newStage {
			_, _ = fmt.Fprintf(s.w, "    %s\n", stage)
		}
		return
	}
	s.mu.Lock()
	s.sub = sub
	s.mu.Unlock()
	if s.program != nil {
		s.program.Send(meshSetMsg{sub: sub})
	}
}

// Done stops the spinner and replaces the frame with a green ✓ summary.
func (s *Spinner) Done() { s.finish(meshDone, nil) }

// Fail stops the spinner and replaces the frame with a red ✗ summary.
func (s *Spinner) Fail(err error) { s.finish(meshFailed, err) }

func (s *Spinner) finish(state meshState, err error) {
	if !s.stopped.CompareAndSwap(false, true) {
		return
	}
	if !s.enabled {
		s.mu.Lock()
		label := s.label
		s.mu.Unlock()
		if state == meshFailed {
			msg := label
			if err != nil {
				msg = fmt.Sprintf("%s: %v", label, err)
			}
			_, _ = fmt.Fprintf(s.w, "  ✗ %s\n", msg)
		} else {
			_, _ = fmt.Fprintf(s.w, "  ✓ %s\n", label)
		}
		return
	}
	if !s.started.Load() || s.program == nil {
		return
	}
	s.program.Send(meshFinishMsg{state: state, err: err})
	<-s.doneCh
}

// Multi fans out reporter ticks to all of rs. Nil entries are skipped.
func Multi(rs ...Reporter) Reporter {
	out := make([]Reporter, 0, len(rs))
	for _, r := range rs {
		if r == nil {
			continue
		}
		out = append(out, r)
	}
	switch len(out) {
	case 0:
		return Nop{}
	case 1:
		return out[0]
	default:
		return multiReporter(out)
	}
}

type multiReporter []Reporter

func (m multiReporter) Report(stage string, current, total int) {
	for _, r := range m {
		r.Report(stage, current, total)
	}
}

// Run animates a spinner around fn. The context passed to fn carries the
// spinner as a Reporter, so any progress.FromContext(ctx).Report(…) inside fn
// drives the sub-status. The spinner is finished (✓ or ✗) before Run returns.
func Run(ctx context.Context, w io.Writer, label string, fn func(context.Context) error) error {
	sp := NewSpinner(w)
	return runWith(ctx, sp, label, fn)
}

// RunDisabled is Run with the spinner forced into plain-text mode.
func RunDisabled(ctx context.Context, w io.Writer, label string, fn func(context.Context) error) error {
	sp := NewSpinner(w)
	sp.Disable()
	return runWith(ctx, sp, label, fn)
}

func runWith(ctx context.Context, sp *Spinner, label string, fn func(context.Context) error) error {
	sp.Start(label)
	ctx = WithReporter(ctx, sp)
	err := fn(ctx)
	if err != nil {
		sp.Fail(err)
	} else {
		sp.Done()
	}
	return err
}

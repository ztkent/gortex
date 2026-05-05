package progress

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Extended palette — used by the post-summary helpers.
var (
	colWarn     = lipgloss.Color("#E8C66B")
	colInfoSoft = lipgloss.Color("#6EAAD4")
	colMuted    = lipgloss.Color("#5A5A60")
	colBorder   = lipgloss.Color("#3A3A40")
)

// Composed styles.
var (
	styleHeading = lipgloss.NewStyle().Foreground(colFgDim).Bold(true)
	styleCount   = lipgloss.NewStyle().Foreground(colMuted)
	styleKey     = lipgloss.NewStyle().Foreground(colFg).Bold(true)
	styleVal     = lipgloss.NewStyle().Foreground(colFgDim)
	styleHint    = lipgloss.NewStyle().Foreground(colMuted).Italic(true)
	styleStep    = lipgloss.NewStyle().Foreground(colInfoSoft)
	styleStrong  = lipgloss.NewStyle().Foreground(colFg).Bold(true)
	styleBox     = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colBorder).
			Padding(0, 1)
)

// Heading returns a section header on its own line: lowercase title,
// optional dim count chip. No leading indent — caller controls layout.
func Heading(title string, count ...string) string {
	t := styleHeading.Render(strings.ToLower(title))
	if len(count) > 0 && count[0] != "" {
		t += "   " + styleCount.Render("· "+count[0])
	}
	return t
}

// Caption renders a one-line dim hint.
func Caption(text string) string {
	return styleHint.Render(text)
}

// Row renders one row of a 2-column listing: bold key (padded to keyWidth)
// followed by dim details. No leading indent.
func Row(key, details string, keyWidth int) string {
	return styleKey.Render(padRight(key, keyWidth)) + "  " + styleVal.Render(details)
}

// Step renders a numbered step ("  1.  text") with the index in soft blue.
func Step(n int, text string) string {
	return styleStep.Render(fmt.Sprintf("%d.", n)) + "  " + styleVal.Render(text)
}

// Chips renders items as " · " separated dim text, fitted to width if width
// is positive (otherwise a single-line list).
func Chips(items []string, width int) string {
	if len(items) == 0 {
		return ""
	}
	if width <= 0 {
		return styleVal.Render(strings.Join(items, " · "))
	}
	// Width-aware multi-line wrap.
	var lines []string
	var cur strings.Builder
	for i, it := range items {
		piece := it
		if i > 0 {
			piece = " · " + piece
		}
		if cur.Len()+lipgloss.Width(piece) > width && cur.Len() > 0 {
			lines = append(lines, cur.String())
			cur.Reset()
			cur.WriteString(it)
		} else {
			cur.WriteString(piece)
		}
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	for i := range lines {
		lines[i] = styleVal.Render(lines[i])
	}
	return strings.Join(lines, "\n")
}

// Stat renders "value unit" with an emphasised value (bold, given color)
// and dim unit. Pass an empty unit to skip the suffix.
type StatSeverity int

const (
	StatNeutral StatSeverity = iota
	StatGood
	StatWarn
	StatBad
)

func Stat(value, unit string, sev StatSeverity) string {
	color := colFg
	switch sev {
	case StatGood:
		color = colAccent
	case StatWarn:
		color = colWarn
	case StatBad:
		color = colErr
	}
	v := lipgloss.NewStyle().Foreground(color).Bold(true).Render(value)
	if unit == "" {
		return v
	}
	return v + " " + styleVal.Render(unit)
}

// StatStrip renders multiple Stats joined with " · ".
func StatStrip(stats ...string) string {
	return strings.Join(stats, styleVal.Render("  ·  "))
}

// Card wraps body in a dim rounded box with an optional title row above. Pad
// is the inner padding rows. Returns the box plus a trailing newline.
func Card(title, body string) string {
	if title != "" {
		body = styleStrong.Render(title) + "\n" + body
	}
	return styleBox.Render(body) + "\n"
}

// Indent prefixes every line of s with the given number of spaces.
func Indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	return pad + strings.ReplaceAll(s, "\n", "\n"+pad)
}

// SortStrings is a small convenience used by callers preparing chip lists.
func SortStrings(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func padRight(s string, n int) string {
	w := lipgloss.Width(s)
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}

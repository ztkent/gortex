package mcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeCRLF(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		want     string
		removals []int
	}{
		{"no terminators", "abc", "abc", nil},
		{"pure LF untouched", "a\nb\n", "a\nb\n", nil},
		{"single CRLF", "a\r\nb", "a\nb", []int{1}},
		{"trailing CRLF", "a\r\n", "a\n", []int{1}},
		{"consecutive CRLF", "\r\n\r\n", "\n\n", []int{0, 1}},
		{"multiple lines", "a\r\nb\r\nc", "a\nb\nc", []int{1, 3}},
		{"lone CR untouched", "a\rb", "a\rb", nil},
		{"lone CR at EOF untouched", "ab\r", "ab\r", nil},
		{"CR before CRLF keeps lone CR", "a\r\r\nb", "a\r\nb", []int{2}},
		{"mixed LF and CRLF", "a\nb\r\nc", "a\nb\nc", []int{3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, removals := normalizeCRLF(tt.in)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.removals, removals)
		})
	}
}

func TestOrigOffset(t *testing.T) {
	// orig "a\r\nb" -> norm "a\nb", removals [1].
	removals := []int{1}
	assert.Equal(t, 0, origOffset(removals, 0), "byte before the removal is unshifted")
	assert.Equal(t, 1, origOffset(removals, 1), "offset on the recorded LF resolves to its removed CR")
	assert.Equal(t, 3, origOffset(removals, 2), "byte after the removal shifts by one")
	assert.Equal(t, 4, origOffset(removals, 3), "end-of-string maps to original length")

	// orig "\r\n\r\n" -> norm "\n\n", removals [0, 1].
	removals = []int{0, 1}
	assert.Equal(t, 0, origOffset(removals, 0))
	assert.Equal(t, 2, origOffset(removals, 1))
	assert.Equal(t, 4, origOffset(removals, 2))

	assert.Equal(t, 5, origOffset(nil, 5), "no removals is identity")
}

func TestFindEOLMatches_ExactLF(t *testing.T) {
	m := findEOLMatches("foo bar foo", "foo")
	assert.Equal(t, 2, m.count)
	assert.False(t, m.normalized)
	assert.Equal(t, []eolSpan{{0, 3}, {8, 11}}, m.spans)
}

func TestFindEOLMatches_NonOverlapping(t *testing.T) {
	m := findEOLMatches("aaa", "aa")
	assert.Equal(t, 1, m.count, "strings.Count semantics: non-overlapping")
	assert.Equal(t, []eolSpan{{0, 2}}, m.spans)
}

func TestFindEOLMatches_EmptyNeedle(t *testing.T) {
	m := findEOLMatches("abc", "")
	assert.Equal(t, 4, m.count, "mirrors strings.Count for the empty needle")
	assert.Empty(t, m.spans)
	assert.False(t, m.normalized)
}

func TestFindEOLMatches_NoMatch(t *testing.T) {
	m := findEOLMatches("hello\r\nworld", "absent")
	assert.Equal(t, 0, m.count)
	assert.Empty(t, m.spans)
	assert.False(t, m.normalized)
}

func TestFindEOLMatches_CRLFHayLFNeedle(t *testing.T) {
	hay := "alpha\r\nbeta\r\ngamma\r\n"
	m := findEOLMatches(hay, "beta\ngamma")
	require.Equal(t, 1, m.count)
	assert.True(t, m.normalized)
	require.Len(t, m.spans, 1)
	assert.Equal(t, "beta\r\ngamma", hay[m.spans[0].start:m.spans[0].end],
		"span must cover the CRLF bytes of the matched region")
}

func TestFindEOLMatches_CRLFHayLFNeedle_Multiple(t *testing.T) {
	hay := "x\r\ny\r\nQ\r\nx\r\ny\r\n"
	m := findEOLMatches(hay, "x\ny\n")
	require.Equal(t, 2, m.count)
	assert.True(t, m.normalized)
	for _, sp := range m.spans {
		assert.Equal(t, "x\r\ny\r\n", hay[sp.start:sp.end])
	}
}

func TestFindEOLMatches_LFHayCRLFNeedle(t *testing.T) {
	hay := "alpha\nbeta\ngamma\n"
	m := findEOLMatches(hay, "beta\r\ngamma")
	require.Equal(t, 1, m.count)
	assert.True(t, m.normalized)
	assert.Equal(t, "beta\ngamma", hay[m.spans[0].start:m.spans[0].end])
}

func TestFindEOLMatches_NeedleStartingWithNewlineExactWins(t *testing.T) {
	// "\nb" is a byte-exact substring of "a\r\nb" — the exact pass wins
	// and the span covers only the bytes the caller named.
	hay := "a\r\nb"
	m := findEOLMatches(hay, "\nb")
	require.Equal(t, 1, m.count)
	assert.False(t, m.normalized)
	assert.Equal(t, "\nb", hay[m.spans[0].start:m.spans[0].end])
}

func TestFindEOLMatches_NeedleStartingWithNewlineSwallowsCR(t *testing.T) {
	// Multi-terminator needle that cannot exact-match: the normalized
	// match starting at a LF must swallow its CR so replacing the span
	// never strands a bare CR.
	hay := "a\r\nb\r\nc"
	m := findEOLMatches(hay, "\nb\nc")
	require.Equal(t, 1, m.count)
	require.True(t, m.normalized)
	assert.Equal(t, "\r\nb\r\nc", hay[m.spans[0].start:m.spans[0].end],
		"a normalized match starting at the LF must swallow the CR")
}

func TestFindEOLMatches_NeedleEndingBeforeNewlineLeavesCR(t *testing.T) {
	hay := "ab\r\nc"
	m := findEOLMatches(hay, "ab")
	require.Equal(t, 1, m.count)
	assert.False(t, m.normalized, "no terminator in the needle: exact match")
	assert.Equal(t, "ab", hay[m.spans[0].start:m.spans[0].end])
}

func TestFindEOLMatches_LoneCRIsNotATerminator(t *testing.T) {
	m := findEOLMatches("foo\rbar", "foo\nbar")
	assert.Equal(t, 0, m.count, "a lone CR must not be treated as CRLF")
}

func TestFindEOLMatches_ExactPreferredOverNormalized(t *testing.T) {
	// Mixed file: the LF section matches byte-exactly; the CRLF section
	// would only match normalized. Exact must win and report one match.
	hay := "a\nb\nQ\r\na\r\nb\r\n"
	m := findEOLMatches(hay, "a\nb\n")
	assert.Equal(t, 1, m.count)
	assert.False(t, m.normalized)
	assert.Equal(t, 0, m.spans[0].start)
}

func TestFindEOLMatches_WholeFile(t *testing.T) {
	hay := "one\r\ntwo\r\n"
	m := findEOLMatches(hay, "one\ntwo\n")
	require.Equal(t, 1, m.count)
	assert.Equal(t, eolSpan{0, len(hay)}, m.spans[0])
}

func TestFindEOLMatches_ConsecutiveBlankCRLFLines(t *testing.T) {
	hay := "a\r\n\r\nb\r\n"
	m := findEOLMatches(hay, "a\n\nb\n")
	require.Equal(t, 1, m.count)
	assert.Equal(t, eolSpan{0, len(hay)}, m.spans[0])
}

func TestSpliceSpansEOL_SingleSpan(t *testing.T) {
	hay := "alpha\r\nbeta\r\ngamma\r\n"
	m := findEOLMatches(hay, "beta\n")
	require.Equal(t, 1, m.count)
	got := spliceSpansEOL(hay, m.spans, "delta\nepsilon\n", 1)
	assert.Equal(t, "alpha\r\ndelta\r\nepsilon\r\ngamma\r\n", got,
		"replacement must be written with the span's CRLF terminators")
}

func TestSpliceSpansEOL_AllSpans(t *testing.T) {
	hay := "x\r\nQ\r\nx\r\n"
	m := findEOLMatches(hay, "x\n")
	require.Equal(t, 2, m.count)
	got := spliceSpansEOL(hay, m.spans, "y\n", -1)
	assert.Equal(t, "y\r\nQ\r\ny\r\n", got)
}

func TestSpliceSpansEOL_LimitOne(t *testing.T) {
	hay := "x\r\nQ\r\nx\r\n"
	m := findEOLMatches(hay, "x\n")
	require.Equal(t, 2, m.count)
	got := spliceSpansEOL(hay, m.spans, "y\n", 1)
	assert.Equal(t, "y\r\nQ\r\nx\r\n", got, "only the first span is replaced")
}

func TestSpliceSpansEOL_MixedFilePerSpanAdaptation(t *testing.T) {
	// One LF region and one CRLF region match the same normalized needle.
	// The needle's endings are mixed so NEITHER region matches exactly.
	// Each replacement must adopt its own region's terminator style.
	hay := "x\ny\nQ\r\nx\r\ny\r\n"
	m := findEOLMatches(hay, "x\r\ny\n")
	require.Equal(t, 2, m.count)
	require.True(t, m.normalized)
	got := spliceSpansEOL(hay, m.spans, "z\nw\n", -1)
	assert.Equal(t, "z\nw\nQ\r\nz\r\nw\r\n", got)
}

func TestSpliceSpansEOL_LFFileStaysLF(t *testing.T) {
	hay := "alpha\nbeta\n"
	m := findEOLMatches(hay, "beta\r\n")
	require.Equal(t, 1, m.count)
	got := spliceSpansEOL(hay, m.spans, "gamma\r\ndelta\r\n", 1)
	assert.Equal(t, "alpha\ngamma\ndelta\n", got,
		"CRLF replacement into an LF region is rewritten to LF")
}

func TestDominantEOL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "\n"},
		{"no newlines", "abc", "\n"},
		{"pure LF", "a\nb\n", "\n"},
		{"pure CRLF", "a\r\nb\r\n", "\r\n"},
		{"CRLF majority", "a\r\nb\r\nc\n", "\r\n"},
		{"LF majority", "a\nb\nc\r\n", "\n"},
		{"tie goes LF", "a\nb\r\n", "\n"},
		{"lone CR ignored", "a\rb\n", "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, dominantEOL(tt.in))
		})
	}
}

func TestAdaptToDominantEOL(t *testing.T) {
	tests := []struct {
		name        string
		replacement string
		context     string
		want        string
	}{
		{"LF into CRLF context", "a\nb\n", "x\r\ny\r\n", "a\r\nb\r\n"},
		{"CRLF into LF context", "a\r\nb\r\n", "x\ny\n", "a\nb\n"},
		{"mixed replacement unified to CRLF", "a\nb\r\nc", "x\r\n", "a\r\nb\r\nc"},
		{"mixed replacement unified to LF", "a\nb\r\nc", "x\n", "a\nb\nc"},
		{"no terminators untouched", "abc", "x\r\n", "abc"},
		{"lone CR passes through", "a\rb", "x\r\n", "a\rb"},
		{"already matching CRLF", "a\r\n", "x\r\n", "a\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, adaptToDominantEOL(tt.replacement, tt.context))
		})
	}
}

func TestMatchSpansHint(t *testing.T) {
	file := "a\nb\nc\nb\nb\n"
	m := findEOLMatches(file, "b\n")
	require.Equal(t, 3, m.count)
	hint := matchSpansHint(file, m.spans)
	assert.Equal(t, " (first match lines 2, 4, 5)", hint)

	assert.Empty(t, matchSpansHint(file, nil))
}

func TestMatchSpansHint_CapsAtThreeWithSuffix(t *testing.T) {
	file := strings.Repeat("b\n", 5)
	m := findEOLMatches(file, "b\n")
	require.Equal(t, 5, m.count)
	hint := matchSpansHint(file, m.spans)
	assert.Equal(t, " (first match lines 1, 2, 3, ...)", hint)
}

func TestMatchSpansHint_CRLFLineNumbers(t *testing.T) {
	file := "one\r\ntwo\r\nthree\r\ntwo\r\n"
	m := findEOLMatches(file, "two\n")
	require.Equal(t, 2, m.count)
	require.True(t, m.normalized)
	hint := matchSpansHint(file, m.spans)
	assert.Equal(t, " (first match lines 2, 4)", hint)
}

func TestMatchLocationsHint_ExactBehaviorPreserved(t *testing.T) {
	file := "\nTODO\nTODO\nTODO\n"
	hint := matchLocationsHint(file, "TODO")
	assert.Equal(t, " (first match lines 2, 3, 4)", hint)

	assert.Empty(t, matchLocationsHint(file, ""))
	assert.Empty(t, matchLocationsHint(file, "absent"))
}

func TestMatchLocationsHint_EOLTolerant(t *testing.T) {
	file := "one\r\nTODO x\r\nTODO x\r\n"
	hint := matchLocationsHint(file, "TODO x\n")
	assert.Equal(t, " (first match lines 2, 3)", hint,
		"hint must report normalized matches too")
}

func TestEOLRoundTrip_NoMixedEndingsAfterSplice(t *testing.T) {
	hay := "func a() {\r\n\tx := 1\r\n\t_ = x\r\n}\r\n"
	m := findEOLMatches(hay, "\tx := 1\n\t_ = x")
	require.Equal(t, 1, m.count)
	require.True(t, m.normalized)
	got := spliceSpansEOL(hay, m.spans, "\ty := 2\n\t_ = y", 1)
	assert.Equal(t, "func a() {\r\n\ty := 2\r\n\t_ = y\r\n}\r\n", got)
	assert.NotContains(t, strings.ReplaceAll(got, "\r\n", ""), "\n",
		"every LF in the result must be part of a CRLF pair")
}

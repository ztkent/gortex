package embedding

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChunkSymbol_ShortFunctionIsOneChunk asserts a function below the
// line threshold is embedded whole — one chunk, no splitting.
func TestChunkSymbol_ShortFunctionIsOneChunk(t *testing.T) {
	src := `func add(a, b int) int {
	return a + b
}`
	chunks := ChunkSymbol([]byte(src), "go", "x.go::add", ChunkOptions{ThresholdLines: 60, WindowLines: 40})
	require.Len(t, chunks, 1, "a short function must produce exactly one chunk")
	assert.Equal(t, src, chunks[0].Text)
	assert.Equal(t, "x.go::add", chunks[0].ParentID)
	assert.Equal(t, 0, chunks[0].WindowIndex)
}

// TestChunkSymbol_LongGoFuncSplitsOnBlocks asserts a long Go function
// is split into multiple windows on its top-level statements, that
// every chunk carries the parent ID and a sequential WindowIndex, and
// that the rejoined windows reproduce the original source exactly.
func TestChunkSymbol_LongGoFuncSplitsOnBlocks(t *testing.T) {
	var b strings.Builder
	b.WriteString("func bigFunc() {\n")
	// 40 single-line statements — well past a 10-line window cap, so
	// the splitter must produce several windows.
	for i := 0; i < 40; i++ {
		b.WriteString("\tx := compute()\n")
	}
	b.WriteString("}")
	src := b.String()

	chunks := ChunkSymbol([]byte(src), "go", "x.go::bigFunc", ChunkOptions{ThresholdLines: 10, WindowLines: 10})
	require.Greater(t, len(chunks), 1, "a long function must split into more than one window")

	rejoined := strings.Builder{}
	for i, c := range chunks {
		assert.Equal(t, "x.go::bigFunc", c.ParentID, "every chunk carries the parent ID")
		assert.Equal(t, i, c.WindowIndex, "WindowIndex must be sequential from 0")
		rejoined.WriteString(c.Text)
	}
	assert.Equal(t, src, rejoined.String(),
		"concatenating the windows must reproduce the symbol source byte-for-byte")
}

// TestChunkSymbol_WindowCapRespected asserts no window (except one
// containing a single oversized statement) exceeds the line cap.
func TestChunkSymbol_WindowCapRespected(t *testing.T) {
	var b strings.Builder
	b.WriteString("func paged() {\n")
	for i := 0; i < 60; i++ {
		b.WriteString("\tstep()\n")
	}
	b.WriteString("}")
	src := b.String()

	const window = 12
	chunks := ChunkSymbol([]byte(src), "go", "x.go::paged", ChunkOptions{ThresholdLines: 20, WindowLines: window})
	require.Greater(t, len(chunks), 1)
	for i, c := range chunks {
		lines := strings.Count(c.Text, "\n") + 1
		// Allow generous slack — the first window also carries the
		// signature line and the last the closing brace; the invariant
		// is that windows are bounded, not exact.
		assert.LessOrEqual(t, lines, window+4,
			"window %d has %d lines, expected near the %d cap", i, lines, window)
	}
}

// TestChunkSymbol_LongTypeSplitsOnFields asserts a large Go struct is
// split on its field declarations.
func TestChunkSymbol_LongTypeSplitsOnFields(t *testing.T) {
	var b strings.Builder
	b.WriteString("type BigStruct struct {\n")
	for i := 0; i < 50; i++ {
		b.WriteString("\tFieldX int\n")
	}
	b.WriteString("}")
	src := b.String()

	chunks := ChunkSymbol([]byte(src), "go", "x.go::BigStruct", ChunkOptions{ThresholdLines: 15, WindowLines: 12})
	require.Greater(t, len(chunks), 1, "a large struct must split on its fields")
	rejoined := strings.Builder{}
	for _, c := range chunks {
		rejoined.WriteString(c.Text)
	}
	assert.Equal(t, src, rejoined.String())
}

// TestChunkSymbol_UnknownLanguageWhole asserts a language with no
// splitter yields a single whole-symbol chunk regardless of size.
func TestChunkSymbol_UnknownLanguageWhole(t *testing.T) {
	src := strings.Repeat("line of cobol\n", 200)
	chunks := ChunkSymbol([]byte(src), "cobol", "x.cob::Thing", ChunkOptions{ThresholdLines: 10, WindowLines: 5})
	require.Len(t, chunks, 1, "no splitter for the language → embed whole")
	assert.Equal(t, src, chunks[0].Text)
}

// TestChunkSymbol_GarbageStillOneChunk asserts a span that fails to
// parse falls back to a single chunk rather than erroring or dropping
// the symbol.
func TestChunkSymbol_GarbageStillOneChunk(t *testing.T) {
	src := strings.Repeat("}{ ][ <<>> ;;;\n", 100)
	chunks := ChunkSymbol([]byte(src), "go", "x.go::junk", ChunkOptions{ThresholdLines: 10, WindowLines: 5})
	require.GreaterOrEqual(t, len(chunks), 1, "a parse failure must still yield at least one chunk")
	// Whatever the split, the windows must still cover the whole input.
	rejoined := strings.Builder{}
	for _, c := range chunks {
		rejoined.WriteString(c.Text)
	}
	assert.Equal(t, src, rejoined.String())
}

// TestChunkSymbol_EmptyInput asserts the empty-span edge case yields a
// single empty chunk.
func TestChunkSymbol_EmptyInput(t *testing.T) {
	chunks := ChunkSymbol(nil, "go", "x.go::empty", ChunkOptions{})
	require.Len(t, chunks, 1)
	assert.Equal(t, "", chunks[0].Text)
}

// TestChunkSymbol_DefaultsApplied asserts zero-valued options fall back
// to the package defaults: a function under DefaultChunkThresholdLines
// stays whole.
func TestChunkSymbol_DefaultsApplied(t *testing.T) {
	var b strings.Builder
	b.WriteString("func midsize() {\n")
	for i := 0; i < DefaultChunkThresholdLines-5; i++ {
		b.WriteString("\tdoThing()\n")
	}
	b.WriteString("}")
	chunks := ChunkSymbol([]byte(b.String()), "go", "x.go::midsize", ChunkOptions{})
	assert.Len(t, chunks, 1, "a function under the default threshold must stay whole")
}

// TestChunkSymbol_LongPythonFuncSplits exercises the indent-language
// path: a long Python function splits on its block statements.
func TestChunkSymbol_LongPythonFuncSplits(t *testing.T) {
	var b strings.Builder
	b.WriteString("def big():\n")
	for i := 0; i < 40; i++ {
		b.WriteString("    x = compute()\n")
	}
	src := b.String()
	chunks := ChunkSymbol([]byte(src), "python", "x.py::big", ChunkOptions{ThresholdLines: 10, WindowLines: 10})
	require.Greater(t, len(chunks), 1, "a long Python function must split")
	rejoined := strings.Builder{}
	for _, c := range chunks {
		rejoined.WriteString(c.Text)
	}
	assert.Equal(t, src, rejoined.String())
}

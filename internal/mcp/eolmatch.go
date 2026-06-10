package mcp

import (
	"fmt"
	"sort"
	"strings"
)

// EOL-tolerant fragment matching for the string-replacement edit tools
// (edit_file, edit_symbol, and the batch_edit ops). Agents routinely author
// old_string / old_source with LF line terminators while the on-disk file
// uses CRLF (or vice versa) — exact byte matching would report "not found"
// even though the text is visually identical. findEOLMatches keeps the
// byte-exact match as the primary path and falls back to a CRLF<->LF
// normalized comparison, mapping every match span back onto the original
// bytes so callers always splice real file offsets.

// eolSpan is a byte range [start, end) in the original (unnormalized) string.
type eolSpan struct {
	start, end int
}

// eolMatches reports where a needle occurs in a haystack.
type eolMatches struct {
	count int
	// spans holds every non-overlapping match in original byte offsets.
	// Empty for the degenerate empty-needle case (count still mirrors
	// strings.Count semantics there).
	spans []eolSpan
	// normalized is true when the spans were produced by the CRLF<->LF
	// fallback rather than byte-exact matching. Callers must rewrite the
	// replacement's line terminators (spliceSpansEOL / adaptToDominantEOL)
	// before splicing so the edit never introduces mixed endings.
	normalized bool
}

// findEOLMatches locates needle in hay: byte-exact first, then — when the
// exact pass finds nothing and either side carries CRLF — with both sides'
// CRLF terminators normalized to LF. Fallback spans are mapped back onto
// hay's original bytes; a match that starts at a normalized LF swallows the
// CR that preceded it, so replacing the span never strands a bare CR.
func findEOLMatches(hay, needle string) eolMatches {
	if needle == "" {
		// Mirror strings.Count: an empty needle "matches" between every
		// byte pair. No spans — no caller splices on an empty needle.
		return eolMatches{count: len(hay) + 1}
	}
	if spans := exactSpans(hay, needle); len(spans) > 0 {
		return eolMatches{count: len(spans), spans: spans}
	}
	if !strings.Contains(hay, "\r\n") && !strings.Contains(needle, "\r\n") {
		return eolMatches{}
	}
	normHay, removals := normalizeCRLF(hay)
	normNeedle, _ := normalizeCRLF(needle)
	nspans := exactSpans(normHay, normNeedle)
	if len(nspans) == 0 {
		return eolMatches{}
	}
	spans := make([]eolSpan, len(nspans))
	for i, sp := range nspans {
		spans[i] = eolSpan{
			start: origOffset(removals, sp.start),
			end:   origOffset(removals, sp.end),
		}
	}
	return eolMatches{count: len(spans), spans: spans, normalized: true}
}

// exactSpans returns every non-overlapping byte-exact match of needle in
// hay, scanning left to right (strings.Count semantics, non-empty needle).
func exactSpans(hay, needle string) []eolSpan {
	var spans []eolSpan
	from := 0
	for {
		idx := strings.Index(hay[from:], needle)
		if idx < 0 {
			return spans
		}
		start := from + idx
		spans = append(spans, eolSpan{start: start, end: start + len(needle)})
		from = start + len(needle)
	}
}

// normalizeCRLF rewrites every CRLF in s to a bare LF. For each removed CR
// it records the index its LF lands on in the normalized output. Lone CRs
// are not line terminators and pass through untouched.
func normalizeCRLF(s string) (string, []int) {
	if !strings.Contains(s, "\r\n") {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	var removals []int
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			// The LF is written on the next iteration at this index.
			removals = append(removals, b.Len())
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String(), removals
}

// origOffset maps an offset in the normalized string back onto the
// original: every CR removed strictly before the offset shifts it right by
// one. An offset sitting exactly on a recorded LF resolves to its removed
// CR, which is what span semantics need — a span starting there swallows
// the CR, and a span ending there leaves the CR to the remainder.
func origOffset(removals []int, off int) int {
	return off + sort.SearchInts(removals, off)
}

// spliceSpansEOL replaces up to n of the given spans (sorted,
// non-overlapping — as produced by findEOLMatches) in hay with replacement,
// rewriting the replacement's line terminators per span to match the bytes
// that span carries. A CRLF region stays CRLF and an LF region stays LF
// even in a mixed-EOL file. n < 0 replaces all spans.
func spliceSpansEOL(hay string, spans []eolSpan, replacement string, n int) string {
	if n >= 0 && n < len(spans) {
		spans = spans[:n]
	}
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		b.WriteString(hay[last:sp.start])
		b.WriteString(adaptToDominantEOL(replacement, hay[sp.start:sp.end]))
		last = sp.end
	}
	b.WriteString(hay[last:])
	return b.String()
}

// dominantEOL reports the line terminator s predominantly uses: CRLF when
// CRLF terminators outnumber bare-LF ones, LF otherwise (including the tie
// and the no-newline cases).
func dominantEOL(s string) string {
	crlf := strings.Count(s, "\r\n")
	if crlf > strings.Count(s, "\n")-crlf {
		return "\r\n"
	}
	return "\n"
}

// adaptToDominantEOL rewrites replacement's line terminators to the
// dominant terminator of context so a normalized-match splice never
// introduces mixed line endings. Lone CRs in the replacement pass through
// untouched.
func adaptToDominantEOL(replacement, context string) string {
	norm := strings.ReplaceAll(replacement, "\r\n", "\n")
	if dominantEOL(context) == "\r\n" {
		return strings.ReplaceAll(norm, "\n", "\r\n")
	}
	return norm
}

// matchSpansHint returns a brief " (first match lines X, Y, Z)" hint for up
// to three match spans. Empty when there are none. Helps an agent choose a
// more unique fragment without re-reading the file.
func matchSpansHint(fileStr string, spans []eolSpan) string {
	if len(spans) == 0 {
		return ""
	}
	const maxHits = 3
	n := min(len(spans), maxHits)
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		// Line number = 1 + count of '\n' before the span. Counting LFs
		// works for CRLF content too — each terminator carries one LF.
		parts[i] = fmt.Sprintf("%d", 1+strings.Count(fileStr[:spans[i].start], "\n"))
	}
	suffix := ""
	if len(spans) > maxHits {
		suffix = ", ..."
	}
	return fmt.Sprintf(" (first match lines %s%s)", strings.Join(parts, ", "), suffix)
}

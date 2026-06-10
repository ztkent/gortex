package mcp

import (
	"sort"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestFindEOLMatches_Property generates haystacks with randomly mixed
// CRLF/LF terminators, derives a needle from a random slice of the
// normalized haystack (so a tolerant match must exist), re-terminates it
// with a random EOL flavor, and checks the matcher's core invariants:
//
//  1. at least one match is found;
//  2. every span's bytes normalize to the normalized needle (the span
//     covers exactly the on-disk text the caller asked for, CRs included);
//  3. spans are in-bounds, sorted, and non-overlapping;
//  4. replacing every span with the needle itself is a normalized no-op
//     (splice offsets land on real bytes, terminators adapt per region).
func TestFindEOLMatches_Property(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		nLines := rapid.IntRange(1, 8).Draw(rt, "nLines")
		var hayB strings.Builder
		for i := 0; i < nLines; i++ {
			// Line content occasionally carries a lone CR — always followed
			// by a letter so it stays lone (a trailing bare CR would fuse
			// with an LF terminator into CRLF, and a needle derived from
			// the once-normalized haystack would then correctly NOT match
			// the \r\r\n bytes). normalizeCRLF must pass lone CRs through
			// and the span/splice invariants must still hold around them.
			hayB.WriteString(rapid.StringMatching(`[a-c]{0,3}(\r[a-c]{1,2})?`).Draw(rt, "line"))
			hayB.WriteString(rapid.SampledFrom([]string{"\n", "\r\n"}).Draw(rt, "eol"))
		}
		hay := hayB.String()

		normHay, _ := normalizeCRLF(hay)
		start := rapid.IntRange(0, len(normHay)-1).Draw(rt, "start")
		end := rapid.IntRange(start+1, len(normHay)).Draw(rt, "end")
		needleNorm := normHay[start:end]
		needle := needleNorm
		if rapid.Bool().Draw(rt, "crlfNeedle") {
			needle = strings.ReplaceAll(needleNorm, "\n", "\r\n")
		}

		m := findEOLMatches(hay, needle)
		if m.count == 0 {
			rt.Fatalf("needle %q derived from hay %q must match", needle, hay)
		}
		if len(m.spans) != m.count {
			rt.Fatalf("count %d != len(spans) %d", m.count, len(m.spans))
		}

		prevEnd := 0
		for i, sp := range m.spans {
			if sp.start < 0 || sp.end > len(hay) || sp.start >= sp.end {
				rt.Fatalf("span %d out of bounds: %+v (hay len %d)", i, sp, len(hay))
			}
			if sp.start < prevEnd {
				rt.Fatalf("span %d overlaps previous (start %d < prev end %d)", i, sp.start, prevEnd)
			}
			prevEnd = sp.end
			gotNorm, _ := normalizeCRLF(hay[sp.start:sp.end])
			if gotNorm != needleNorm {
				rt.Fatalf("span %d bytes %q normalize to %q, want %q",
					i, hay[sp.start:sp.end], gotNorm, needleNorm)
			}
		}
		if !sort.SliceIsSorted(m.spans, func(i, j int) bool { return m.spans[i].start < m.spans[j].start }) {
			rt.Fatalf("spans not sorted: %+v", m.spans)
		}

		out := spliceSpansEOL(hay, m.spans, needleNorm, -1)
		outNorm, _ := normalizeCRLF(out)
		if outNorm != normHay {
			rt.Fatalf("identity replacement changed normalized content:\nhay  %q\nout  %q", hay, out)
		}
	})
}

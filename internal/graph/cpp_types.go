package graph

import "strings"

// NormalizeCppType reduces a C++ type spelling to a stable comparison key used
// by both the extractor (stamping parameter types) and the overload resolver
// (normalizing call-site argument hints), so the two always compare in the same
// space. It strips template arguments, cv-qualifiers, ref/ptr punctuation, and
// namespace qualifiers, and canonicalises a few stdlib aliases — while keeping
// the integer/float ladder distinct (int vs long) so genuinely different
// overloads stay rankable.
func NormalizeCppType(raw string) string {
	s := stripCppTemplateArgs(raw)
	s = strings.ReplaceAll(s, "&&", " ")
	s = strings.ReplaceAll(s, "&", " ")
	s = strings.ReplaceAll(s, "*", " ")
	fields := strings.Fields(s)
	kept := fields[:0]
	for _, f := range fields {
		if f == "const" || f == "volatile" {
			continue
		}
		kept = append(kept, f)
	}
	s = strings.Join(kept, " ")
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	s = strings.TrimSpace(s)
	switch s {
	case "string", "basic_string", "string_view":
		return "string"
	case "unsigned", "unsigned int", "uint", "size_t", "uint32_t", "uint64_t":
		return "unsigned"
	}
	return s
}

func stripCppTemplateArgs(s string) string {
	depth := 0
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

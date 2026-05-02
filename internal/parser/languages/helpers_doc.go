package languages

import (
	"strings"
)

// docMaxLen caps the stored doc comment length. Per spec-graph-detail.md
// §4.1: 400 chars covers a typical first paragraph (~80 tokens) without
// bloating GCX1 responses.
const docMaxLen = 400

// docCommentLang controls which prefix-strip rule the helper applies.
// The helper is reused across languages with the same line-comment
// syntax but different leading-prefix conventions.
type docCommentLang int

const (
	// DocLangSlashSlash strips a leading "//" (and optional space).
	// Captures Go, JavaScript, TypeScript, Rust (///, //!), C/C++,
	// C#, Swift, Dart, Kotlin, Scala line comments.
	DocLangSlashSlash docCommentLang = iota
	// DocLangBlockStar handles JSDoc/Javadoc/PHPDoc style /** ... */
	// blocks plus a fallback to // single-line comments above the
	// declaration.
	DocLangBlockStar
	// DocLangHash strips a leading "#" (and optional space).
	// Captures Python (line comments above defs), Ruby, Bash, R,
	// Makefile.
	DocLangHash
	// DocLangCSharpXML strips C# XML doc markers (/// <summary>...).
	DocLangCSharpXML
)

// ExtractDocAbove walks upward from startRow0 collecting contiguous
// comment lines that sit above the declaration, and returns the first
// paragraph as a single line, truncated to docMaxLen. startRow0 is the
// 0-based row of the declaration's first line (matching tree-sitter's
// row numbering).
//
// "Contiguous" means no blank line and no non-comment line between the
// last collected comment line and the declaration. A blank or
// non-comment line terminates the scan upward.
//
// Returns "" when no leading comment is found. Safe to call on every
// emit — the cost per call is O(comment-block-size).
func ExtractDocAbove(src []byte, startRow0 int, lang docCommentLang) string {
	if startRow0 <= 0 || len(src) == 0 {
		return ""
	}
	lines := splitLinesUpTo(src, startRow0)
	if len(lines) == 0 {
		return ""
	}

	// Walk upward from the line just above the declaration.
	collected := make([]string, 0, 8)
	inBlock := false
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], "\r")
		trimmed := strings.TrimSpace(line)

		switch lang {
		case DocLangSlashSlash:
			if trimmed == "" {
				if len(collected) == 0 {
					continue
				}
				goto done
			}
			if strings.HasPrefix(trimmed, "//") {
				collected = append(collected, stripLineCommentPrefix(trimmed, "//"))
				continue
			}
			goto done

		case DocLangBlockStar:
			// Match `*/` end → walk into block. Match `/**` start →
			// finish block. Otherwise treat as // line comments
			// fallback.
			if !inBlock && strings.HasSuffix(trimmed, "*/") {
				body := strings.TrimSuffix(trimmed, "*/")
				body = strings.TrimSpace(body)
				if strings.HasPrefix(trimmed, "/**") {
					// Single-line /** ... */ block.
					inner := strings.TrimPrefix(body, "/**")
					inner = strings.TrimSpace(inner)
					if inner != "" {
						collected = append(collected, inner)
					}
					goto done
				}
				inBlock = true
				if body != "" {
					collected = append(collected, stripBlockStarLine(body))
				}
				continue
			}
			if inBlock {
				if strings.HasPrefix(trimmed, "/**") {
					body := strings.TrimPrefix(trimmed, "/**")
					body = strings.TrimSpace(body)
					if body != "" {
						collected = append(collected, stripBlockStarLine(body))
					}
					goto done
				}
				collected = append(collected, stripBlockStarLine(trimmed))
				continue
			}
			if trimmed == "" {
				if len(collected) == 0 {
					continue
				}
				goto done
			}
			if strings.HasPrefix(trimmed, "//") {
				collected = append(collected, stripLineCommentPrefix(trimmed, "//"))
				continue
			}
			goto done

		case DocLangHash:
			if trimmed == "" {
				if len(collected) == 0 {
					continue
				}
				goto done
			}
			if strings.HasPrefix(trimmed, "#") {
				collected = append(collected, stripLineCommentPrefix(trimmed, "#"))
				continue
			}
			goto done

		case DocLangCSharpXML:
			if trimmed == "" {
				if len(collected) == 0 {
					continue
				}
				goto done
			}
			if strings.HasPrefix(trimmed, "///") {
				collected = append(collected, stripCSharpXMLLine(trimmed))
				continue
			}
			goto done
		}
	}

done:
	if len(collected) == 0 {
		return ""
	}
	// collected is in reverse order (we walked upward). Reverse.
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return firstParagraph(collected)
}

// ExtractPyDocstring extracts a Python docstring: the first string
// literal in the function/class body. bodyText is the raw source text
// of the suite/block node. Returns the first paragraph (text up to the
// first blank line), truncated to docMaxLen. Returns "" if no
// docstring.
func ExtractPyDocstring(bodyText string) string {
	s := strings.TrimLeft(bodyText, " \t\r\n")
	if s == "" {
		return ""
	}
	// Triple-quoted forms first.
	for _, q := range []string{`"""`, `'''`} {
		if strings.HasPrefix(s, q) {
			rest := s[3:]
			end := strings.Index(rest, q)
			if end < 0 {
				return ""
			}
			doc := strings.TrimSpace(rest[:end])
			return firstPyParagraph(doc)
		}
	}
	// Single-quoted single-line docstrings (rare but valid).
	for _, q := range []string{`"`, `'`} {
		if strings.HasPrefix(s, q) {
			rest := s[1:]
			end := strings.Index(rest, q)
			if end < 0 {
				return ""
			}
			line := strings.TrimSpace(rest[:end])
			if line == "" {
				return ""
			}
			return truncateDoc(line)
		}
	}
	return ""
}

// firstPyParagraph collapses the first paragraph of a Python docstring
// to a single line, truncated to docMaxLen. A "paragraph" is text up
// to the first blank-line gap.
func firstPyParagraph(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		ln := strings.TrimSpace(line)
		if ln == "" {
			if b.Len() > 0 {
				break
			}
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(ln)
		if b.Len() > docMaxLen {
			break
		}
	}
	return truncateDoc(b.String())
}

// splitLinesUpTo returns lines[0..upToRow] (0-based, exclusive of the
// declaration's own row). Does not allocate the full file split when
// upToRow is small.
func splitLinesUpTo(src []byte, upToRow int) []string {
	if upToRow <= 0 {
		return nil
	}
	lines := make([]string, 0, upToRow)
	start := 0
	row := 0
	for i := 0; i < len(src) && row < upToRow; i++ {
		if src[i] != '\n' {
			continue
		}
		lines = append(lines, string(src[start:i]))
		start = i + 1
		row++
	}
	return lines
}

func stripLineCommentPrefix(line, prefix string) string {
	s := strings.TrimPrefix(line, prefix)
	// Rust /// and //! both fall through after the // strip.
	s = strings.TrimPrefix(s, "/")
	s = strings.TrimPrefix(s, "!")
	s = strings.TrimPrefix(s, " ")
	return s
}

func stripBlockStarLine(line string) string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "*")
	s = strings.TrimPrefix(s, " ")
	return s
}

func stripCSharpXMLLine(line string) string {
	s := strings.TrimPrefix(line, "///")
	s = strings.TrimPrefix(s, " ")
	// Drop XML tags (very rough — keeps text content). The spec calls
	// for <summary> contents only; we don't parse XML here, just strip
	// angle-bracketed tokens.
	var b strings.Builder
	depth := 0
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
	return strings.TrimSpace(b.String())
}

// firstParagraph joins collected lines with a single space, stops at
// the first blank-line gap (already filtered) or a JSDoc/Javadoc
// `@param`/`@return`/etc. tag, and truncates to docMaxLen.
func firstParagraph(lines []string) string {
	var b strings.Builder
	for i, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			if b.Len() > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(ln, "@") {
			// JSDoc/Javadoc tag — first paragraph ended.
			break
		}
		if i > 0 && b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(ln)
		if b.Len() > docMaxLen {
			break
		}
	}
	return truncateDoc(b.String())
}

func truncateDoc(s string) string {
	if len(s) <= docMaxLen {
		return s
	}
	// Cut on a rune boundary.
	cut := docMaxLen
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// --- Visibility ----------------------------------------------------

// VisibilityPublic / VisibilityPrivate / etc. are the canonical values
// for Node.Meta["visibility"]. Per spec-graph-detail.md §4.2.
const (
	VisibilityPublic    = "public"
	VisibilityPrivate   = "private"
	VisibilityProtected = "protected"
	VisibilityInternal  = "internal"
	VisibilityPackage   = "package"
)

// VisibilityByCase returns "public" for Go-style identifiers (first
// rune uppercase ASCII) and "package" otherwise. Used by Go.
func VisibilityByCase(name string) string {
	if name == "" {
		return ""
	}
	c := name[0]
	if c >= 'A' && c <= 'Z' {
		return VisibilityPublic
	}
	return VisibilityPackage
}

// VisibilityByUnderscore returns "private" for names starting with "_"
// and "public" otherwise. Used by Python and Dart.
func VisibilityByUnderscore(name string) string {
	if name == "" {
		return ""
	}
	if name[0] == '_' {
		return VisibilityPrivate
	}
	return VisibilityPublic
}

// VisibilityFromModifiers picks the strongest known modifier from a
// list, with `defaultVis` as the fallback. Recognized modifiers:
// public, private, protected, internal, open (kotlin → public),
// fileprivate (swift → private), pub (rust → public),
// "pub(crate)" (rust → internal).
func VisibilityFromModifiers(modifiers []string, defaultVis string) string {
	for _, m := range modifiers {
		switch strings.TrimSpace(m) {
		case "public", "open", "pub":
			return VisibilityPublic
		case "private", "fileprivate":
			return VisibilityPrivate
		case "protected":
			return VisibilityProtected
		case "internal", "pub(crate)":
			return VisibilityInternal
		case "package", "package-private":
			return VisibilityPackage
		}
	}
	return defaultVis
}

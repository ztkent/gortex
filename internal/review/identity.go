package review

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// IdentityKey computes a stable, line-drift-invariant identity for a finding.
//
// The key is a SHA-256 over the tuple
//
//	rule + category + normalized(file) + symbol + content-anchor(source line)
//
// deliberately EXCLUDING the line number: the same flagged code dismissed at
// line 10 keeps the same key after the file shifts it to line 40, so a
// suppression survives unrelated edits above it. Two findings differ in key
// whenever any of rule / category / symbol / normalized-file / trimmed source
// text differs.
//
// When the generator did not pass the flagged line's text (Finding.SourceLine
// empty) the content anchor falls back to the empty string, so the key reduces
// to (rule, category, file, symbol) — coarser, and liable to over-suppress
// sibling findings on the same symbol, but still stable.
func IdentityKey(f Finding) string {
	h := sha256.New()
	// A newline separator that cannot appear inside any single component keeps
	// the concatenation unambiguous (a trailing/leading boundary can't migrate
	// content from one field into another).
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{'\n'})
		}
	}
	write(
		strings.TrimSpace(f.Rule),
		normalizeCategory(f.Category),
		normalizeIdentityPath(f.File),
		strings.TrimSpace(f.SymbolID),
		lineContentAnchor(f.File, f.Line, f.SourceLine),
	)
	return hex.EncodeToString(h.Sum(nil))
}

// lineContentAnchor derives the drift-invariant content component of the
// identity key from the flagged line's source text. It folds away leading /
// trailing whitespace and collapses interior runs of whitespace to a single
// space, so a reflow / re-indent of the same statement keeps the same anchor.
// The file path and line number are accepted for symmetry with the spec'd
// signature and to let a future anchor strategy fold them in; the line number
// is intentionally NOT hashed here — that is what makes the key line-stable.
// An empty source line yields an empty anchor (the coarse fallback).
func lineContentAnchor(file string, line int, src string) string {
	_ = file
	_ = line
	return strings.Join(strings.Fields(src), " ")
}

// normalizeIdentityPath canonicalises a file path to forward-slash form and
// strips a leading "./" so two spellings of the same path produce one key.
func normalizeIdentityPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = filepath.ToSlash(filepath.Clean(p))
	return strings.TrimPrefix(p, "./")
}

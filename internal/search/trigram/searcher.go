package trigram

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Match is one line of one file that contains the query literal.
type Match struct {
	Path string `json:"path"` // forward-slash repo-relative path
	Line int    `json:"line"` // 1-based line number
	Text string `json:"text"` // the matching line
}

// Searcher is a trigram-accelerated literal code search over a fixed
// set of files. Build it once against a repo's file list, then Grep it
// repeatedly. It is safe for concurrent Grep calls.
type Searcher struct {
	root  string
	ix    *Index
	paths []string // docID -> forward-slash repo-relative path
}

// Build reads every file — forward-slash repo-relative paths under
// root — and indexes its content. A file that cannot be read is left
// unindexed (it never matches) but keeps its docID slot so the rest
// stay aligned.
func Build(root string, relPaths []string) *Searcher {
	s := &Searcher{
		root:  root,
		ix:    New(),
		paths: make([]string, len(relPaths)),
	}
	for i, rel := range relPaths {
		rel = filepath.ToSlash(rel)
		s.paths[i] = rel
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		s.ix.Add(uint32(i), content)
	}
	return s
}

// Grep returns up to limit lines, across the indexed files, that
// contain the literal query. The trigram index narrows the file set;
// each candidate file is then scanned to confirm the match and locate
// its lines. Results are ordered by file, then by line. A non-positive
// limit returns every match.
func (s *Searcher) Grep(query string, limit int) []Match {
	if query == "" {
		return nil
	}
	var matches []Match
	for _, docID := range s.ix.Candidates(query) {
		if int(docID) >= len(s.paths) {
			continue
		}
		rel := s.paths[docID]
		f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			text := scanner.Text()
			if strings.Contains(text, query) {
				matches = append(matches, Match{Path: rel, Line: line, Text: text})
				if limit > 0 && len(matches) >= limit {
					_ = f.Close()
					return matches
				}
			}
		}
		_ = f.Close()
	}
	return matches
}

// DocCount returns the number of indexed files.
func (s *Searcher) DocCount() int { return s.ix.DocCount() }

// GrepRegexp returns up to limit lines, across the indexed files, that
// the compiled regexp re matches. requiredLiterals is a set of literal
// substrings (each ideally >= 3 bytes) that every matching line's file
// must contain — they come from the regex's own mandatory literal runs
// and let the trigram index narrow the candidate file set. When
// requiredLiterals is empty no trigram pre-filter is possible and every
// indexed file is scanned. Results are ordered by file, then by line.
// A non-positive limit returns every match.
//
// pathPrefix, when non-empty, restricts the scan to files whose
// forward-slash repo-relative path starts with it.
func (s *Searcher) GrepRegexp(re *regexp.Regexp, requiredLiterals []string, pathPrefix string, limit int) []Match {
	if re == nil {
		return nil
	}

	// Build the candidate doc set. Each required literal intersects its
	// trigram posting list into the running set; the first literal
	// seeds it. With no usable literal we fall back to every doc.
	var candidates map[uint32]struct{}
	for _, lit := range requiredLiterals {
		if len(lit) < 3 {
			// Too short to trigram-filter — skip; the regex scan still
			// verifies, so correctness is unaffected.
			continue
		}
		got := make(map[uint32]struct{})
		for _, id := range s.ix.Candidates(lit) {
			got[id] = struct{}{}
		}
		if candidates == nil {
			candidates = got
			continue
		}
		for id := range candidates {
			if _, ok := got[id]; !ok {
				delete(candidates, id)
			}
		}
	}

	var docIDs []uint32
	if candidates == nil {
		docIDs = make([]uint32, len(s.paths))
		for i := range s.paths {
			docIDs[i] = uint32(i)
		}
	} else {
		docIDs = make([]uint32, 0, len(candidates))
		for id := range candidates {
			docIDs = append(docIDs, id)
		}
		sort.Slice(docIDs, func(a, b int) bool { return docIDs[a] < docIDs[b] })
	}

	var matches []Match
	for _, docID := range docIDs {
		if int(docID) >= len(s.paths) {
			continue
		}
		rel := s.paths[docID]
		if pathPrefix != "" && !strings.HasPrefix(rel, pathPrefix) {
			continue
		}
		f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			text := scanner.Text()
			if re.MatchString(text) {
				matches = append(matches, Match{Path: rel, Line: line, Text: text})
				if limit > 0 && len(matches) >= limit {
					_ = f.Close()
					return matches
				}
			}
		}
		_ = f.Close()
	}
	return matches
}

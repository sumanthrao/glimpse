package gitbackend

import (
	"context"
	"regexp"
	"sort"
	"strings"
)

// GrepResult is the structured output of Backend.Grep — designed to be
// returned to an agent so it can see what was actually searched and recover
// when the search was incomplete.
type GrepResult struct {
	Matches []GrepMatch  `json:"matches"`
	Search  GrepStrategy `json:"searched"`
	Note    string       `json:"note,omitempty"`
}

// GrepStrategy describes which sources contributed to a Grep call.
type GrepStrategy struct {
	CodeSearch  CodeSearchStrategy  `json:"code_search"`
	LocalIndex  LocalIndexStrategy  `json:"local_index"`
}

type CodeSearchStrategy struct {
	Used       bool     `json:"used"`
	Literals   []string `json:"literals,omitempty"`
	Candidates int      `json:"candidates"`
	Downloaded int      `json:"downloaded"`
	Skipped    string   `json:"skipped,omitempty"` // human reason if Used=false
}

type LocalIndexStrategy struct {
	Files   int `json:"files"`
	Matches int `json:"matches"`
}

// Grep searches for pattern across the repo. It merges hits from the local
// IncrementalIndex (covers the working set) with hits from GitHub Code Search
// (covers everything we haven't downloaded yet), downloading Code Search
// candidates in parallel and running the regex on their bytes.
//
// pathScope, if non-empty, restricts results to paths starting with it.
//
// The returned GrepResult always carries metadata describing what was
// searched, so an agent can decide whether the result was complete.
func (b *Backend) Grep(ctx context.Context, pattern, pathScope string) (*GrepResult, error) {
	pathScope = NormalizePath(pathScope)
	if pattern == "" {
		return nil, errEmptyPattern
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	// 1. Local index search — covers the working set with full regex
	//    semantics. Always runs, even when the pattern has no literals.
	localMatches, err := b.idx.Search(pattern, pathScope)
	if err != nil {
		return nil, err
	}
	idxStats := b.idx.Stats()

	res := &GrepResult{
		Matches: append([]GrepMatch(nil), localMatches...),
		Search: GrepStrategy{
			LocalIndex: LocalIndexStrategy{
				Files:   idxStats.Files,
				Matches: len(localMatches),
			},
		},
	}

	// 2. Code Search for everything we haven't downloaded yet. Requires at
	//    least one literal anchor of length >= 3.
	literals := ExtractLiterals(pattern)
	if len(literals) == 0 {
		res.Search.CodeSearch.Skipped = "pattern has no literal anchors (3+ chars)"
		if idxStats.Files == 0 {
			res.Note = "Pattern has no literal anchors and no files have been read yet. " +
				"Add a literal substring (3+ chars) to the pattern (e.g. 'handleAuth'), " +
				"or call read_file on relevant files first to expand the searchable working set."
		} else {
			res.Note = "Pattern has no literal anchors; only the local working set " +
				"(" + itoa(idxStats.Files) + " files) was searched. " +
				"Add a 3+ char literal substring to also search the full repo via Code Search."
		}
		return res, nil
	}

	hits, err := b.gh.codeSearch(ctx, b.Ref.Owner, b.Ref.Repo, literals, pathScope)
	if err != nil {
		// Soft fail — return what we have from the local index plus the error
		// note. Agent can retry with a token if rate-limited.
		res.Search.CodeSearch.Used = true
		res.Search.CodeSearch.Literals = literals
		res.Note = "Code Search failed: " + err.Error() + ". Showing local working-set matches only."
		return res, nil
	}

	// Filter out paths already covered by the local index — saves redundant
	// downloads and prevents duplicate matches.
	var fetchPaths []string
	seenPath := map[string]struct{}{}
	for _, m := range res.Matches {
		seenPath[m.Path] = struct{}{}
	}
	for _, h := range hits {
		if pathScope != "" && !strings.HasPrefix(h.Path, pathScope) {
			continue
		}
		if b.idx.Has(h.Path) {
			continue
		}
		if _, dup := seenPath[h.Path]; dup {
			continue
		}
		fetchPaths = append(fetchPaths, h.Path)
	}

	res.Search.CodeSearch = CodeSearchStrategy{
		Used:       true,
		Literals:   literals,
		Candidates: len(hits),
		Downloaded: len(fetchPaths),
	}

	if len(fetchPaths) == 0 {
		return res, nil
	}

	files, _ := b.AccessFiles(ctx, fetchPaths)

	// Run the actual regex on the downloaded bytes.
	for _, p := range fetchPaths {
		data, ok := files[p]
		if !ok {
			continue
		}
		start := 0
		line := 1
		for i := 0; i <= len(data); i++ {
			if i == len(data) || data[i] == '\n' {
				if i > start {
					if re.Match(data[start:i]) {
						res.Matches = append(res.Matches, GrepMatch{
							Path: p, Line: line,
							Text: string(data[start:i]),
							Via:  "code_search",
						})
					}
				}
				start = i + 1
				line++
			}
		}
	}

	// Stable order: by path then line.
	sort.Slice(res.Matches, func(i, j int) bool {
		if res.Matches[i].Path != res.Matches[j].Path {
			return res.Matches[i].Path < res.Matches[j].Path
		}
		return res.Matches[i].Line < res.Matches[j].Line
	})

	return res, nil
}

var errEmptyPattern = stringError("pattern is empty; pass a regex like 'handleAuth' or 'func.*Login'")

type stringError string

func (e stringError) Error() string { return string(e) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	if n < 0 {
		return "-" + itoa(-n)
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

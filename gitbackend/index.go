package gitbackend

import (
	"bytes"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// IncrementalIndex is a trigram inverted index over the working set — every
// blob the backend has fetched at least once, deduped by blob SHA.
//
// It is grown incrementally by AccessFile, never as a bulk pre-warm step.
// Searches use posting-list intersection on the pattern's literals to narrow
// to candidate files, then run regex on those candidates' bytes.
type IncrementalIndex struct {
	mu       sync.RWMutex
	paths    []string             // file index -> path
	contents [][]byte             // file index -> bytes (skipped for binary)
	pathID   map[string]int32     // path -> file index
	shaSeen  map[string]int32     // blob SHA -> first file index that stored it
	posting  map[[3]byte][]int32  // trigram -> list of file indexes (sorted)
}

func newIncrementalIndex() *IncrementalIndex {
	return &IncrementalIndex{
		pathID:  map[string]int32{},
		shaSeen: map[string]int32{},
		posting: map[[3]byte][]int32{},
	}
}

// IndexStats summarizes the current index for repo_status.
type IndexStats struct {
	Files    int
	Bytes    int64
	Trigrams int
}

func (idx *IncrementalIndex) Stats() IndexStats {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	var bytesTotal int64
	for _, c := range idx.contents {
		bytesTotal += int64(len(c))
	}
	return IndexStats{Files: len(idx.paths), Bytes: bytesTotal, Trigrams: len(idx.posting)}
}

// Has reports whether a path is already indexed (binary files are NOT indexed
// even though they may be cached at the backend layer).
func (idx *IncrementalIndex) Has(path string) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	_, ok := idx.pathID[path]
	return ok
}

// Add inserts (path, sha, data) into the index. Binary content is skipped
// (paths still recorded as "binary" so future Has() returns true and we don't
// re-attempt indexing). If the same SHA was added under a different path, we
// alias to the existing entry — same content, two paths, one set of postings.
func (idx *IncrementalIndex) Add(path, sha string, data []byte) {
	if path == "" {
		return
	}

	if isBinary(data) {
		idx.mu.Lock()
		if _, ok := idx.pathID[path]; !ok {
			idx.pathID[path] = -1 // sentinel: known but not searchable
		}
		idx.mu.Unlock()
		return
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	if _, ok := idx.pathID[path]; ok {
		return
	}

	// Same content under another path? Alias the postings.
	if existing, ok := idx.shaSeen[sha]; ok && existing >= 0 {
		fileIdx := int32(len(idx.paths))
		idx.paths = append(idx.paths, path)
		idx.contents = append(idx.contents, idx.contents[existing])
		idx.pathID[path] = fileIdx
		// reuse postings: extract trigrams from existing content, append fileIdx
		// (cheaper than re-scanning by walking known postings, but we don't
		// keep a per-file trigram set; just rescan the bytes once. Worth the
		// constant simplicity given typical file sizes.)
		idx.addPostings(fileIdx, idx.contents[existing])
		return
	}

	fileIdx := int32(len(idx.paths))
	idx.paths = append(idx.paths, path)
	idx.contents = append(idx.contents, data)
	idx.pathID[path] = fileIdx
	if sha != "" {
		idx.shaSeen[sha] = fileIdx
	}
	idx.addPostings(fileIdx, data)
}

func (idx *IncrementalIndex) addPostings(fileIdx int32, data []byte) {
	if len(data) < 3 {
		return
	}
	seen := make(map[[3]byte]struct{}, len(data)/4)
	for i := 0; i+3 <= len(data); i++ {
		var tri [3]byte
		copy(tri[:], data[i:i+3])
		if _, ok := seen[tri]; ok {
			continue
		}
		seen[tri] = struct{}{}
		idx.posting[tri] = append(idx.posting[tri], fileIdx)
	}
}

// GrepMatch is one hit returned by Search/grep flows.
type GrepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
	Via  string `json:"via,omitempty"` // "local_index" | "code_search" | "code_search_snippet"
}

// Search runs a regex against the indexed working set, narrowed first by
// trigrams from the pattern's literal substrings. If the pattern has no usable
// literals, every indexed file is regex-scanned (still bounded by working-set
// size).
//
// pathScope, if non-empty, restricts results to paths starting with it.
func (idx *IncrementalIndex) Search(pattern, pathScope string) ([]GrepMatch, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	literals := ExtractLiterals(pattern)
	trigrams := literalsToTrigrams(literals)

	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var candidates []int32
	if len(trigrams) > 0 {
		candidates = idx.intersect(trigrams)
	} else {
		candidates = make([]int32, 0, len(idx.paths))
		for i, c := range idx.contents {
			if len(c) > 0 {
				candidates = append(candidates, int32(i))
			}
		}
	}

	var out []GrepMatch
	for _, fi := range candidates {
		if int(fi) >= len(idx.paths) {
			continue
		}
		path := idx.paths[fi]
		if pathScope != "" && !strings.HasPrefix(path, pathScope) {
			continue
		}
		data := idx.contents[fi]
		if len(data) == 0 {
			continue
		}
		// Walk lines once.
		start := 0
		line := 1
		for i := 0; i <= len(data); i++ {
			if i == len(data) || data[i] == '\n' {
				if i > start {
					if re.Match(data[start:i]) {
						out = append(out, GrepMatch{
							Path: path, Line: line,
							Text: string(data[start:i]),
							Via:  "local_index",
						})
					}
				}
				start = i + 1
				line++
			}
		}
	}
	return out, nil
}

// intersect returns the file IDs that appear in every trigram's posting list,
// starting from the rarest trigram for cheapness.
func (idx *IncrementalIndex) intersect(trigrams [][3]byte) []int32 {
	if len(trigrams) == 0 {
		return nil
	}
	shortest := 0
	for i := 1; i < len(trigrams); i++ {
		if len(idx.posting[trigrams[i]]) < len(idx.posting[trigrams[shortest]]) {
			shortest = i
		}
	}
	if len(idx.posting[trigrams[shortest]]) == 0 {
		return nil
	}

	set := make(map[int32]struct{}, len(idx.posting[trigrams[shortest]]))
	for _, fi := range idx.posting[trigrams[shortest]] {
		set[fi] = struct{}{}
	}
	for i, tri := range trigrams {
		if i == shortest {
			continue
		}
		other := idx.posting[tri]
		othSet := make(map[int32]struct{}, len(other))
		for _, fi := range other {
			othSet[fi] = struct{}{}
		}
		for fi := range set {
			if _, ok := othSet[fi]; !ok {
				delete(set, fi)
			}
		}
	}

	out := make([]int32, 0, len(set))
	for fi := range set {
		out = append(out, fi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ExtractLiterals pulls literal runs (length >= 3) out of a regex pattern.
// Used both by the local index for trigram narrowing and by Code Search to
// build a server-side query.
func ExtractLiterals(pattern string) []string {
	const meta = `.+*?[](){}|^$`

	var runs []string
	var cur strings.Builder

	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '\\' && i+1 < len(pattern) {
			cur.WriteByte(pattern[i+1])
			i++
			continue
		}
		if strings.IndexByte(meta, c) >= 0 {
			if cur.Len() >= 3 {
				runs = append(runs, cur.String())
			}
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if cur.Len() >= 3 {
		runs = append(runs, cur.String())
	}
	return runs
}

func literalsToTrigrams(lits []string) [][3]byte {
	seen := map[[3]byte]struct{}{}
	var out [][3]byte
	for _, run := range lits {
		for i := 0; i+3 <= len(run); i++ {
			var tri [3]byte
			copy(tri[:], run[i:i+3])
			if _, ok := seen[tri]; ok {
				continue
			}
			seen[tri] = struct{}{}
			out = append(out, tri)
		}
	}
	return out
}

func isBinary(data []byte) bool {
	peek := data
	if len(peek) > 8192 {
		peek = peek[:8192]
	}
	return bytes.IndexByte(peek, 0) >= 0
}

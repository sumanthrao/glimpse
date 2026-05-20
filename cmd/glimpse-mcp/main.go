// glimpse-mcp: An MCP server that lets AI agents glimpse into git repos directly from
// the object store. Zero external dependencies — just the Go stdlib and the
// git CLI you already have installed. All reads are cached in memory.
//
// Optimizations:
//   - Persistent git cat-file --batch process for blob reads (no process spawn per read)
//   - Tree cache: ls-tree results are cached for the session (fixed ref = immutable trees)
//   - Blob cache: file contents cached after first read
//   - Trigram index (--index): pre-built in-memory index for sub-10ms grep
//
// Usage:
//
//	go build -o glimpse-mcp ./cmd/glimpse-mcp
//	echo '...' | ./glimpse-mcp --repo /path/to/repo
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// JSON-RPC message types (just enough for MCP over stdio)
// ---------------------------------------------------------------------------

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// MCP protocol types (minimal subset)
// ---------------------------------------------------------------------------

type initResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    any    `json:"capabilities"`
	ServerInfo      any    `json:"serverInfo"`
}

type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ---------------------------------------------------------------------------
// Persistent git cat-file --batch reader
// ---------------------------------------------------------------------------

type batchReader struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
}

func newBatchReader(repoDir string) (*batchReader, error) {
	cmd := exec.Command("git", "-C", repoDir, "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &batchReader{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 1<<20),
	}, nil
}

// Read fetches an object by specifier (e.g. "HEAD:path/to/file").
// Returns the raw content bytes. Thread-safe.
func (b *batchReader) Read(spec string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, err := fmt.Fprintf(b.stdin, "%s\n", spec); err != nil {
		return nil, fmt.Errorf("write to cat-file: %w", err)
	}

	header, err := b.stdout.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	header = strings.TrimSpace(header)

	if strings.HasSuffix(header, " missing") {
		return nil, fmt.Errorf("not found")
	}

	fields := strings.Fields(header)
	if len(fields) < 3 {
		return nil, fmt.Errorf("unexpected header: %s", header)
	}

	size, err := strconv.Atoi(fields[2])
	if err != nil {
		return nil, fmt.Errorf("bad size in header: %s", header)
	}

	// content is <size> bytes followed by a trailing LF
	buf := make([]byte, size+1)
	if _, err := io.ReadFull(b.stdout, buf); err != nil {
		return nil, fmt.Errorf("read content: %w", err)
	}

	return buf[:size], nil
}

func (b *batchReader) Close() {
	b.stdin.Close()
	b.cmd.Wait()
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

type server struct {
	repoDir string
	ref     string

	mu        sync.Mutex
	blobCache map[string][]byte
	treeCache map[string]string

	batch *batchReader
	index *trigramIndex // nil if --index not used
}

func main() {
	repo := flag.String("repo", "", "Path to git repository (auto-detected from cwd if omitted)")
	ref := flag.String("ref", "HEAD", "Git ref to serve (branch, tag, commit)")
	indexFlag := flag.Bool("index", false, "Build a trigram index on startup for sub-10ms grep")
	flag.Parse()

	if *repo == "" {
		dir, _ := os.Getwd()
		for {
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
				*repo = dir
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				die("not inside a git repo and --repo not specified")
			}
			dir = parent
		}
	}

	abs, err := filepath.Abs(*repo)
	if err != nil {
		die("resolve path: %v", err)
	}
	if err := exec.Command("git", "-C", abs, "rev-parse", "--git-dir").Run(); err != nil {
		die("%s is not a git repository", abs)
	}

	batch, err := newBatchReader(abs)
	if err != nil {
		die("start cat-file --batch: %v", err)
	}
	defer batch.Close()

	s := &server{
		repoDir:   abs,
		ref:       *ref,
		blobCache: make(map[string][]byte),
		treeCache: make(map[string]string),
		batch:     batch,
	}

	if *indexFlag {
		idx, err := buildTrigramIndex(batch, abs, *ref)
		if err != nil {
			die("build index: %v", err)
		}
		s.index = idx
	}

	fmt.Fprintf(os.Stderr, "glimpse MCP server (zero deps, git CLI)\n")
	fmt.Fprintf(os.Stderr, "  repo:  %s\n", abs)
	fmt.Fprintf(os.Stderr, "  ref:   %s\n", *ref)
	if s.index != nil {
		fmt.Fprintf(os.Stderr, "  index: %d files\n", len(s.index.paths))
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 10<<20), 10<<20)

	enc := json.NewEncoder(os.Stdout)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}

		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}

		resp := response{JSONRPC: "2.0", ID: req.ID}

		switch req.Method {
		case "initialize":
			resp.Result = initResult{
				ProtocolVersion: "2024-11-05",
				Capabilities:    map[string]any{"tools": map[string]any{}},
				ServerInfo:      map[string]any{"name": "glimpse", "version": "1.0.0"},
			}
		case "tools/list":
			resp.Result = map[string]any{"tools": s.toolDefs()}
		case "tools/call":
			resp.Result = s.dispatchTool(req.Params)
		default:
			resp.Error = map[string]any{"code": -32601, "message": "method not found: " + req.Method}
		}

		_ = enc.Encode(resp)
	}
}

// ---------------------------------------------------------------------------
// Tool definitions
// ---------------------------------------------------------------------------

func (s *server) toolDefs() []toolDef {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}

	return []toolDef{
		{
			Name:        "list_directory",
			Description: "List files and directories at a path in the git repository",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": str("Directory path relative to repo root (empty for root)")},
			},
		},
		{
			Name:        "read_file",
			Description: "Read file content from the git object store (cached in memory after first read)",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": str("File path relative to repo root")},
				"required":   []string{"path"},
			},
		},
		{
			Name:        "write_file",
			Description: "Write content to a file in the worktree (creates parent directories as needed)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    str("File path relative to repo root"),
					"content": str("File content to write"),
				},
				"required": []string{"path", "content"},
			},
		},
		{
			Name:        "file_info",
			Description: "Get file metadata (size, type, mode) without reading the full content",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": str("Path relative to repo root")},
				"required":   []string{"path"},
			},
		},
		{
			Name:        "grep",
			Description: "Search file contents (uses in-memory trigram index when available, otherwise git grep)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": str("Search pattern (regex)"),
					"path":    str("Limit search to this directory/file (optional)"),
				},
				"required": []string{"pattern"},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Tool dispatch
// ---------------------------------------------------------------------------

func (s *server) dispatchTool(raw json.RawMessage) toolResult {
	var p callToolParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult("invalid params: " + err.Error())
	}

	var args struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Content string `json:"content"`
	}
	if len(p.Arguments) > 0 {
		_ = json.Unmarshal(p.Arguments, &args)
	}

	switch p.Name {
	case "list_directory":
		return s.listDirectory(args.Path)
	case "read_file":
		return s.readFile(args.Path)
	case "write_file":
		return s.writeFile(args.Path, args.Content)
	case "file_info":
		return s.fileInfo(args.Path)
	case "grep":
		return s.grep(args.Pattern, args.Path)
	default:
		return errResult("unknown tool: " + p.Name)
	}
}

// ---------------------------------------------------------------------------
// Tool implementations
// ---------------------------------------------------------------------------

// listDirectory runs `git ls-tree -l` with results cached for the session.
func (s *server) listDirectory(path string) toolResult {
	path = normPath(path)
	cacheKey := "ls:" + path

	raw, err := s.cachedTree(cacheKey, func() (string, error) {
		args := []string{"-C", s.repoDir, "ls-tree", "-l", s.ref}
		if path != "" {
			args = append(args, "--", path+"/")
		}
		return gitCmd(args...)
	})
	if err != nil {
		return errResult(fmt.Sprintf("not found: %s", path))
	}

	var sb strings.Builder
	prefix := ""
	if path != "" {
		prefix = path + "/"
	}

	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		tab := strings.SplitN(line, "\t", 2)
		if len(tab) != 2 {
			continue
		}
		name := strings.TrimPrefix(tab[1], prefix)
		meta := strings.Fields(tab[0])
		if len(meta) < 4 {
			continue
		}
		if meta[1] == "tree" {
			fmt.Fprintf(&sb, "dir  %s/\n", name)
		} else {
			fmt.Fprintf(&sb, "file %s (%s bytes)\n", name, meta[3])
		}
	}

	if sb.Len() == 0 {
		return errResult(fmt.Sprintf("empty or not found: %s", path))
	}
	return textResult(sb.String())
}

// readFile uses the persistent cat-file --batch process, with blob caching.
func (s *server) readFile(path string) toolResult {
	if path == "" {
		return errResult("path is required")
	}

	s.mu.Lock()
	if cached, ok := s.blobCache[path]; ok {
		s.mu.Unlock()
		return textResult(string(cached))
	}
	s.mu.Unlock()

	data, err := s.batch.Read(s.ref + ":" + path)
	if err != nil {
		return errResult(fmt.Sprintf("not found: %s", path))
	}

	s.mu.Lock()
	s.blobCache[path] = data
	s.mu.Unlock()

	return textResult(string(data))
}

// writeFile writes content to the worktree and invalidates the cache.
func (s *server) writeFile(path, content string) toolResult {
	if path == "" {
		return errResult("path is required")
	}
	if strings.Contains(path, "..") {
		return errResult("path must not contain '..'")
	}

	fullPath := filepath.Join(s.repoDir, path)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return errResult(fmt.Sprintf("create directory: %v", err))
	}

	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		return errResult(fmt.Sprintf("write file: %v", err))
	}

	s.mu.Lock()
	delete(s.blobCache, path)
	s.mu.Unlock()

	return textResult(fmt.Sprintf("wrote %d bytes to %s", len(content), path))
}

// fileInfo uses cached ls-tree results to get metadata.
func (s *server) fileInfo(path string) toolResult {
	if path == "" {
		return errResult("path is required")
	}

	cacheKey := "info:" + path
	raw, err := s.cachedTree(cacheKey, func() (string, error) {
		return gitCmd("-C", s.repoDir, "ls-tree", "-l", s.ref, "--", path)
	})
	if err != nil || strings.TrimSpace(raw) == "" {
		return errResult(fmt.Sprintf("not found: %s", path))
	}

	line := strings.TrimSpace(raw)
	tab := strings.SplitN(line, "\t", 2)
	if len(tab) != 2 {
		return errResult("unexpected git output")
	}

	meta := strings.Fields(tab[0])
	if len(meta) < 4 {
		return errResult("unexpected git output")
	}

	if meta[1] == "tree" {
		return textResult(fmt.Sprintf("type: directory\npath: %s", path))
	}

	mode := "regular"
	switch meta[0] {
	case "100755":
		mode = "executable"
	case "120000":
		mode = "symlink"
	}

	return textResult(fmt.Sprintf("type: file\npath: %s\nsize: %s bytes\nmode: %s", path, meta[3], mode))
}

// grep uses the trigram index when available, falls back to git grep.
func (s *server) grep(pattern, path string) toolResult {
	if pattern == "" {
		return errResult("pattern is required")
	}

	if s.index != nil {
		return s.index.search(pattern, path)
	}

	args := []string{"-C", s.repoDir, "grep", "-n", "-I", pattern, s.ref}
	if path != "" {
		args = append(args, "--", path)
	}

	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return textResult("no matches found")
	}

	prefix := s.ref + ":"
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	var sb strings.Builder
	for _, line := range lines {
		sb.WriteString(strings.TrimPrefix(line, prefix))
		sb.WriteByte('\n')
	}

	return textResult(sb.String())
}

// ---------------------------------------------------------------------------
// Trigram index — pre-built in-memory index for sub-10ms grep
// ---------------------------------------------------------------------------

type trigramIndex struct {
	paths    []string
	contents [][]byte
	posting  map[[3]byte][]int32
}

func buildTrigramIndex(batch *batchReader, repoDir, ref string) (*trigramIndex, error) {
	start := time.Now()
	fmt.Fprintf(os.Stderr, "building trigram index...\n")

	out, err := gitCmd("-C", repoDir, "ls-tree", "-r", "--name-only", ref)
	if err != nil {
		return nil, fmt.Errorf("ls-tree -r: %w", err)
	}

	allPaths := strings.Split(strings.TrimSpace(out), "\n")
	idx := &trigramIndex{
		posting: make(map[[3]byte][]int32, 1<<16),
	}

	for i, path := range allPaths {
		if path == "" {
			continue
		}

		data, err := batch.Read(ref + ":" + path)
		if err != nil {
			continue
		}

		// skip binary files
		peek := data
		if len(peek) > 8192 {
			peek = peek[:8192]
		}
		if bytes.IndexByte(peek, 0) >= 0 {
			continue
		}

		fileIdx := int32(len(idx.paths))
		idx.paths = append(idx.paths, path)
		idx.contents = append(idx.contents, data)

		seen := make(map[[3]byte]bool)
		for j := 0; j <= len(data)-3; j++ {
			tri := [3]byte{data[j], data[j+1], data[j+2]}
			if !seen[tri] {
				seen[tri] = true
				idx.posting[tri] = append(idx.posting[tri], fileIdx)
			}
		}

		if (i+1)%2000 == 0 {
			fmt.Fprintf(os.Stderr, "  indexed %d/%d files\n", i+1, len(allPaths))
		}
	}

	fmt.Fprintf(os.Stderr, "  done: %d text files indexed in %s\n", len(idx.paths), time.Since(start).Round(time.Millisecond))
	return idx, nil
}

func (idx *trigramIndex) search(pattern, pathPrefix string) toolResult {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return errResult("invalid regex: " + err.Error())
	}

	trigrams := extractTrigrams(pattern)

	var candidates []int32
	if len(trigrams) > 0 {
		candidates = idx.intersect(trigrams)
	} else {
		candidates = make([]int32, len(idx.paths))
		for i := range candidates {
			candidates[i] = int32(i)
		}
	}

	var sb strings.Builder
	for _, fi := range candidates {
		path := idx.paths[fi]
		if pathPrefix != "" && !strings.HasPrefix(path, pathPrefix) {
			continue
		}
		lines := strings.Split(string(idx.contents[fi]), "\n")
		for lineNo, line := range lines {
			if re.MatchString(line) {
				fmt.Fprintf(&sb, "%s:%d:%s\n", path, lineNo+1, line)
			}
		}
	}

	if sb.Len() == 0 {
		return textResult("no matches found")
	}
	return textResult(sb.String())
}

func (idx *trigramIndex) intersect(trigrams [][3]byte) []int32 {
	if len(trigrams) == 0 {
		return nil
	}

	// start with the shortest posting list
	shortest := 0
	for i := range trigrams {
		if len(idx.posting[trigrams[i]]) < len(idx.posting[trigrams[shortest]]) {
			shortest = i
		}
	}

	set := make(map[int32]bool, len(idx.posting[trigrams[shortest]]))
	for _, fi := range idx.posting[trigrams[shortest]] {
		set[fi] = true
	}

	for i, tri := range trigrams {
		if i == shortest {
			continue
		}
		other := make(map[int32]bool, len(idx.posting[tri]))
		for _, fi := range idx.posting[tri] {
			other[fi] = true
		}
		for fi := range set {
			if !other[fi] {
				delete(set, fi)
			}
		}
	}

	result := make([]int32, 0, len(set))
	for fi := range set {
		result = append(result, fi)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// extractTrigrams pulls literal 3-byte sequences from a search pattern,
// skipping regex metacharacters. Used to narrow candidate files before
// doing the actual regexp match.
func extractTrigrams(pattern string) [][3]byte {
	const meta = `.+*?[](){}|^$`

	var runs []string
	var cur strings.Builder

	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '\\' && i+1 < len(pattern) {
			cur.WriteByte(pattern[i+1])
			i++
			continue
		}
		if strings.IndexByte(meta, pattern[i]) >= 0 {
			if cur.Len() >= 3 {
				runs = append(runs, cur.String())
			}
			cur.Reset()
			continue
		}
		cur.WriteByte(pattern[i])
	}
	if cur.Len() >= 3 {
		runs = append(runs, cur.String())
	}

	seen := make(map[[3]byte]bool)
	var out [][3]byte
	for _, run := range runs {
		for j := 0; j <= len(run)-3; j++ {
			tri := [3]byte{run[j], run[j+1], run[j+2]}
			if !seen[tri] {
				seen[tri] = true
				out = append(out, tri)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// cachedTree returns a cached ls-tree result or calls fn and caches it.
func (s *server) cachedTree(key string, fn func() (string, error)) (string, error) {
	s.mu.Lock()
	if cached, ok := s.treeCache[key]; ok {
		s.mu.Unlock()
		return cached, nil
	}
	s.mu.Unlock()

	result, err := fn()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.treeCache[key] = result
	s.mu.Unlock()

	return result, nil
}

func normPath(path string) string {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	if path == "." {
		return ""
	}
	return path
}

func textResult(text string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: text}}}
}

func errResult(msg string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: "error: " + msg}}, IsError: true}
}

func gitCmd(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return string(out), err
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

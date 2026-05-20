// glimpse-mcp: An MCP server that lets AI agents glimpse into git repos directly from
// the object store. Zero external dependencies — just the Go stdlib and the
// git CLI you already have installed. All reads are cached in memory.
//
// Modes:
//   - Single repo:  glimpse-mcp --repo /path/to/repo [--index]
//   - Multi repo:   glimpse-mcp  (agents call open_repo dynamically)
//
// Optimizations:
//   - Persistent git cat-file --batch process for blob reads (no process spawn per read)
//   - Tree cache: ls-tree results are cached for the session (fixed ref = immutable trees)
//   - Blob cache: file contents cached after first read
//   - Trigram index: pre-built in-memory index for sub-10ms grep (always on for open_repo)
//
// Usage:
//
//	go build -o glimpse-mcp ./cmd/glimpse-mcp
//	echo '...' | ./glimpse-mcp                        # multi-repo mode
//	echo '...' | ./glimpse-mcp --repo /path/to/repo   # single-repo mode
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
// Server — handles one repo session
// ---------------------------------------------------------------------------

type server struct {
	repoDir string
	ref     string
	bare    bool // true if opened via bare clone (no worktree)

	mu        sync.Mutex
	blobCache map[string][]byte
	treeCache map[string]string

	batch *batchReader
	index *trigramIndex
}

func (s *server) Close() {
	if s.batch != nil {
		s.batch.Close()
	}
}

// openServer creates a server for the given repo directory.
// If buildIndex is true, a trigram index is built on open.
func openServer(repoDir, ref string, bare, buildIndex bool) (*server, error) {
	batch, err := newBatchReader(repoDir)
	if err != nil {
		return nil, fmt.Errorf("start cat-file --batch: %w", err)
	}

	s := &server{
		repoDir:   repoDir,
		ref:       ref,
		bare:      bare,
		blobCache: make(map[string][]byte),
		treeCache: make(map[string]string),
		batch:     batch,
	}

	if buildIndex {
		idx, err := buildTrigramIndex(batch, repoDir, ref)
		if err != nil {
			batch.Close()
			return nil, fmt.Errorf("build index: %w", err)
		}
		s.index = idx
	}

	return s, nil
}

// ---------------------------------------------------------------------------
// Mux — multiplexes across repos, handles open_repo
// ---------------------------------------------------------------------------

type mux struct {
	cacheDir   string
	indexFlag  bool

	mu      sync.Mutex
	current *server
}

func (m *mux) toolDefs() []toolDef {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}

	return []toolDef{
		{
			Name:        "open_repo",
			Description: "Open a git repo for exploration. Accepts a clone URL (bare-cloned and cached locally) or a local path. Builds a trigram index automatically for sub-10ms grep.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": str("Git clone URL (https or ssh) or absolute local path"),
					"ref": str("Branch, tag, or commit to serve (default: HEAD)"),
				},
				"required": []string{"url"},
			},
		},
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
			Description: "Write content to a file in the worktree (only available for local repos, not bare clones)",
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

func (m *mux) dispatchTool(raw json.RawMessage) toolResult {
	var p callToolParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult("invalid params: " + err.Error())
	}

	if p.Name == "open_repo" {
		var args struct {
			URL string `json:"url"`
			Ref string `json:"ref"`
		}
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments, &args)
		}
		if args.URL == "" {
			return errResult("url is required")
		}
		if args.Ref == "" {
			args.Ref = "HEAD"
		}
		return m.openRepo(args.URL, args.Ref)
	}

	m.mu.Lock()
	s := m.current
	m.mu.Unlock()

	if s == nil {
		return errResult("no repo open — call open_repo first, or start with --repo")
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

func (m *mux) openRepo(urlOrPath, ref string) toolResult {
	start := time.Now()

	repoDir, bare, err := m.resolveRepo(urlOrPath)
	if err != nil {
		return errResult(err.Error())
	}

	s, err := openServer(repoDir, ref, bare, true)
	if err != nil {
		return errResult(fmt.Sprintf("open %s: %v", urlOrPath, err))
	}

	m.mu.Lock()
	old := m.current
	m.current = s
	m.mu.Unlock()

	if old != nil {
		old.Close()
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "repo:  %s\n", repoDir)
	fmt.Fprintf(&sb, "ref:   %s\n", ref)
	if bare {
		fmt.Fprintf(&sb, "mode:  bare clone (read-only)\n")
	} else {
		fmt.Fprintf(&sb, "mode:  local worktree\n")
	}
	if s.index != nil {
		fmt.Fprintf(&sb, "index: %d files\n", len(s.index.paths))
	}
	fmt.Fprintf(&sb, "ready in %s\n", time.Since(start).Round(time.Millisecond))

	fmt.Fprintf(os.Stderr, "  opened: %s (%d files indexed, %s)\n",
		urlOrPath, len(s.index.paths), time.Since(start).Round(time.Millisecond))

	return textResult(sb.String())
}

// resolveRepo resolves a URL or local path to a git directory.
// Returns (repoDir, isBare, error).
func (m *mux) resolveRepo(urlOrPath string) (string, bool, error) {
	if isGitURL(urlOrPath) {
		dir, err := m.cloneOrFetch(urlOrPath)
		if err != nil {
			return "", false, err
		}
		return dir, true, nil
	}

	abs, err := filepath.Abs(urlOrPath)
	if err != nil {
		return "", false, fmt.Errorf("resolve path: %w", err)
	}
	if err := exec.Command("git", "-C", abs, "rev-parse", "--git-dir").Run(); err != nil {
		return "", false, fmt.Errorf("%s is not a git repository", abs)
	}
	return abs, false, nil
}

func (m *mux) cloneOrFetch(url string) (string, error) {
	dir := m.repoCacheDir(url)

	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		fmt.Fprintf(os.Stderr, "  cached: %s → fetch\n", dir)
		cmd := exec.Command("git", "-C", dir, "fetch", "--quiet", "origin")
		cmd.Stderr = os.Stderr
		_ = cmd.Run()
		return dir, nil
	}

	fmt.Fprintf(os.Stderr, "  cloning: %s\n", url)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("git", "clone", "--bare", "--quiet", url, dir)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git clone --bare: %w", err)
	}
	return dir, nil
}

func (m *mux) repoCacheDir(url string) string {
	name := url
	name = strings.TrimSuffix(name, ".git")
	r := strings.NewReplacer("://", "-", "/", "-", ":", "-", "@", "-")
	name = r.Replace(name)
	return filepath.Join(m.cacheDir, name)
}

func isGitURL(s string) bool {
	return strings.Contains(s, "://") || strings.HasPrefix(s, "git@")
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	repo := flag.String("repo", "", "Path to git repository (optional — agents can call open_repo instead)")
	ref := flag.String("ref", "HEAD", "Git ref to serve (branch, tag, commit)")
	indexFlag := flag.Bool("index", false, "Build trigram index on startup (always on for open_repo)")
	cacheDir := flag.String("cache-dir", "", "Where to store bare clones (default: ~/.cache/glimpse)")
	flag.Parse()

	if *cacheDir == "" {
		home, _ := os.UserHomeDir()
		*cacheDir = filepath.Join(home, ".cache", "glimpse")
	}

	m := &mux{
		cacheDir:  *cacheDir,
		indexFlag: *indexFlag,
	}

	// If --repo given, pre-open it (backward compat)
	if *repo != "" {
		abs, err := resolveRepoPath(*repo)
		if err != nil {
			die("%v", err)
		}
		s, err := openServer(abs, *ref, false, *indexFlag)
		if err != nil {
			die("%v", err)
		}
		defer s.Close()
		m.current = s
	}

	fmt.Fprintf(os.Stderr, "glimpse MCP server\n")
	fmt.Fprintf(os.Stderr, "  cache: %s\n", *cacheDir)
	if m.current != nil {
		fmt.Fprintf(os.Stderr, "  repo:  %s\n", m.current.repoDir)
		fmt.Fprintf(os.Stderr, "  ref:   %s\n", m.current.ref)
		if m.current.index != nil {
			fmt.Fprintf(os.Stderr, "  index: %d files\n", len(m.current.index.paths))
		}
	} else {
		fmt.Fprintf(os.Stderr, "  (no repo — agents should call open_repo)\n")
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
				ServerInfo:      map[string]any{"name": "glimpse", "version": "2.0.0"},
			}
		case "tools/list":
			resp.Result = map[string]any{"tools": m.toolDefs()}
		case "tools/call":
			resp.Result = m.dispatchTool(req.Params)
		default:
			resp.Error = map[string]any{"code": -32601, "message": "method not found: " + req.Method}
		}

		_ = enc.Encode(resp)
	}
}

func resolveRepoPath(repo string) (string, error) {
	if repo == "" {
		dir, _ := os.Getwd()
		for {
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				return "", fmt.Errorf("not inside a git repo and --repo not specified")
			}
			dir = parent
		}
	}

	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if err := exec.Command("git", "-C", abs, "rev-parse", "--git-dir").Run(); err != nil {
		return "", fmt.Errorf("%s is not a git repository", abs)
	}
	return abs, nil
}

// ---------------------------------------------------------------------------
// Tool implementations
// ---------------------------------------------------------------------------

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

func (s *server) writeFile(path, content string) toolResult {
	if s.bare {
		return errResult("write_file is not available for bare-cloned repos (read-only) — clone locally to write")
	}
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
	fmt.Fprintf(os.Stderr, "  building trigram index...\n")

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
			fmt.Fprintf(os.Stderr, "    indexed %d/%d files\n", i+1, len(allPaths))
		}
	}

	fmt.Fprintf(os.Stderr, "    %d text files, %s\n", len(idx.paths), time.Since(start).Round(time.Millisecond))
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

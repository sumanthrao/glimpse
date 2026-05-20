// glimpse-mcp: An MCP server that lets AI agents glimpse into git repos directly from
// the object store. Zero external dependencies — just the Go stdlib and the
// git CLI you already have installed. All reads are cached in memory.
//
// Optimizations:
//   - Persistent git cat-file --batch process for blob reads (no process spawn per read)
//   - Tree cache: ls-tree results are cached for the session (fixed ref = immutable trees)
//   - Blob cache: file contents cached after first read
//
// Usage:
//
//	go build -o glimpse-mcp ./cmd/glimpse-mcp
//	echo '...' | ./glimpse-mcp --repo /path/to/repo
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
}

func main() {
	repo := flag.String("repo", "", "Path to git repository (auto-detected from cwd if omitted)")
	ref := flag.String("ref", "HEAD", "Git ref to serve (branch, tag, commit)")
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

	fmt.Fprintf(os.Stderr, "glimpse MCP server (zero deps, git CLI)\n")
	fmt.Fprintf(os.Stderr, "  repo: %s\n", abs)
	fmt.Fprintf(os.Stderr, "  ref:  %s\n", *ref)

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
			Description: "Search file contents in the repository using git grep",
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

// grep runs `git grep` (still spawns a process — grep is inherently variable).
func (s *server) grep(pattern, path string) toolResult {
	if pattern == "" {
		return errResult("pattern is required")
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

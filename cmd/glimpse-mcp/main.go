// glimpse-mcp: An MCP server that lets AI agents glimpse into git repos directly from
// the object store. Zero external dependencies — just the Go stdlib and the
// git CLI you already have installed. All reads are cached in memory.
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
	"os"
	"os/exec"
	"path/filepath"
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
// Server
// ---------------------------------------------------------------------------

type server struct {
	repoDir string
	ref     string
	mu      sync.Mutex
	cache   map[string][]byte
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

	s := &server{repoDir: abs, ref: *ref, cache: make(map[string][]byte)}

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
			continue // notification, no response needed
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
	}
	if len(p.Arguments) > 0 {
		_ = json.Unmarshal(p.Arguments, &args)
	}

	switch p.Name {
	case "list_directory":
		return s.listDirectory(args.Path)
	case "read_file":
		return s.readFile(args.Path)
	case "file_info":
		return s.fileInfo(args.Path)
	case "grep":
		return s.grep(args.Pattern, args.Path)
	default:
		return errResult("unknown tool: " + p.Name)
	}
}

// ---------------------------------------------------------------------------
// Tool implementations — all backed by git CLI
// ---------------------------------------------------------------------------

// listDirectory runs `git ls-tree -l <ref> [-- path/]` and formats the output.
func (s *server) listDirectory(path string) toolResult {
	args := []string{"-C", s.repoDir, "ls-tree", "-l", s.ref}
	if path != "" && path != "." {
		path = strings.TrimSuffix(path, "/")
		args = append(args, "--", path+"/")
	}

	out, err := gitCmd(args...)
	if err != nil {
		return errResult(fmt.Sprintf("not found: %s", path))
	}

	var sb strings.Builder
	prefix := ""
	if path != "" && path != "." {
		prefix = path + "/"
	}

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		// format: <mode> <type> <hash> <size>\t<name>
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

// readFile runs `git show <ref>:<path>` and caches the result.
func (s *server) readFile(path string) toolResult {
	if path == "" {
		return errResult("path is required")
	}

	s.mu.Lock()
	if cached, ok := s.cache[path]; ok {
		s.mu.Unlock()
		return textResult(string(cached))
	}
	s.mu.Unlock()

	out, err := exec.Command("git", "-C", s.repoDir, "show", s.ref+":"+path).Output()
	if err != nil {
		return errResult(fmt.Sprintf("not found: %s", path))
	}

	s.mu.Lock()
	s.cache[path] = out
	s.mu.Unlock()

	return textResult(string(out))
}

// fileInfo runs `git ls-tree -l <ref> -- <path>` to get metadata.
func (s *server) fileInfo(path string) toolResult {
	if path == "" {
		return errResult("path is required")
	}

	out, err := gitCmd("-C", s.repoDir, "ls-tree", "-l", s.ref, "--", path)
	if err != nil || strings.TrimSpace(out) == "" {
		return errResult(fmt.Sprintf("not found: %s", path))
	}

	line := strings.TrimSpace(out)
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

// grep runs `git grep -n -I <pattern> <ref> [-- path]`.
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

	// strip the ref prefix from each line: "HEAD:path:line" → "path:line"
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

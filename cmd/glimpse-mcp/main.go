// Command glimpse-mcp is the Model Context Protocol server for glimpse.
//
// It is a thin shell around gitbackend: every tool routes through the
// AccessFile bridge or its grep / git wrappers. There is no local caching
// layer here — the backend owns the RAM cache, the working-set index, the
// rate-limit state, and the lazy worktree.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/surao/gitfs-accelerator/gitbackend"
)

// ---------------------------------------------------------------------------
// JSON-RPC + MCP types
// ---------------------------------------------------------------------------

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

type initResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    any    `json:"capabilities"`
	ServerInfo      any    `json:"serverInfo"`
	Instructions    string `json:"instructions,omitempty"`
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
// Server state
// ---------------------------------------------------------------------------

type server struct {
	cacheDir string
	token    string

	mu      sync.Mutex
	current *gitbackend.Backend
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

const version = "3.0.0"

func main() {
	cacheDir := flag.String("cache-dir", "", "Cache for partial clones (default: ~/.cache/glimpse)")
	token := flag.String("github-token", "", "GitHub token; defaults to $GITHUB_TOKEN")
	printConfig := flag.Bool("print-mcp-config", false, "Print a sample MCP client config and exit")
	flag.Parse()

	if *printConfig {
		printMCPConfig()
		return
	}

	if *cacheDir == "" {
		home, _ := os.UserHomeDir()
		*cacheDir = filepath.Join(home, ".cache", "glimpse")
	}
	if *token == "" {
		*token = os.Getenv("GITHUB_TOKEN")
	}

	srv := &server{cacheDir: *cacheDir, token: *token}

	fmt.Fprintf(os.Stderr, "glimpse-mcp %s\n", version)
	fmt.Fprintf(os.Stderr, "  cache: %s\n", *cacheDir)
	if *token != "" {
		fmt.Fprintln(os.Stderr, "  auth:  GITHUB_TOKEN set")
	} else {
		fmt.Fprintln(os.Stderr, "  auth:  none (rate limits: 10 search/min, 60 REST/hr)")
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
				ServerInfo:      map[string]any{"name": "glimpse", "version": version},
				Instructions:    agentInstructions,
			}
		case "tools/list":
			resp.Result = map[string]any{"tools": toolDefs()}
		case "tools/call":
			resp.Result = srv.dispatch(req.Params)
		default:
			resp.Error = map[string]any{"code": -32601, "message": "method not found: " + req.Method}
		}
		_ = enc.Encode(resp)
	}
}

// ---------------------------------------------------------------------------
// Tool registry
// ---------------------------------------------------------------------------

func toolDefs() []toolDef {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}

	return []toolDef{
		{
			Name: "open_repo",
			Description: "Open a github.com repo for exploration. Cost: 1 Trees API call (~300 ms; " +
				"a few extra calls when pinning to a subtree). Call this once at the start of a " +
				"session before any other tool. Returns repo metadata, languages, file count, and " +
				"rate-limit state.\n\n" +
				"URL accepts:\n" +
				"  https://github.com/owner/repo\n" +
				"  https://github.com/owner/repo.git\n" +
				"  https://github.com/owner/repo/tree/<ref>\n" +
				"  https://github.com/owner/repo/tree/<ref>/<path>      (pins to a subtree)\n" +
				"  https://github.com/owner/repo/blob/<ref>/<file>      (pins to file's parent dir)\n" +
				"  git@github.com:owner/repo.git\n\n" +
				"Use subtree pinning on monorepos too big for GitHub's ~7 MB / 100k-entry tree cap. " +
				"All subsequent tool paths are then relative to the pinned subtree.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": str("github.com URL (https or ssh). Required."),
					"ref": str("Branch, tag, or commit. Empty = default branch."),
				},
				"required": []string{"url"},
			},
		},
		{
			Name: "list_directory",
			Description: "List children of a directory. Cost: free (in-memory tree). " +
				"Use to navigate the repo structure. Empty path = repo root.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": str("Directory path relative to repo root.")},
			},
		},
		{
			Name: "find_files",
			Description: "Find files whose path matches a glob (or substring). Cost: free. " +
				"Examples: '*.go', 'src/**/*.ts', 'README'. Use to locate files by name before read_file.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": str("Glob pattern. Bare strings match as substring."),
					"path":    str("Limit search to this subtree (optional)."),
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name: "file_info",
			Description: "Metadata for a path (size, type, mode, blob SHA). Cost: free. No content fetched.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": str("Path relative to repo root.")},
				"required":   []string{"path"},
			},
		},
		{
			Name: "read_file",
			Description: "Read a file. Cost: 1 raw.githubusercontent.com fetch on first read (~100 ms); cached after. " +
				"Use this when you know which file you need.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": str("Path relative to repo root."),
				},
				"required": []string{"path"},
			},
		},
		{
			Name: "grep",
			Description: "Search file contents. Cost: 1 GitHub Code Search call + parallel CDN fetches for candidate files. " +
				"Best results: include a literal substring (3+ chars) like 'handleAuth' or 'package main'. " +
				"Pure-regex patterns ('.*', '\\w+') only search the working set already in memory. " +
				"Returns matches plus a 'searched' object describing what was scanned.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": str("Regex pattern. Include literal anchors for full-repo coverage."),
					"path":    str("Limit search to this directory/file (optional)."),
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name: "write_file",
			Description: "Write content to a file. First call triggers a one-time partial clone + worktree (~2-8 s). " +
				"Subsequent writes are local. Reads of unmodified files still go through the CDN.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    str("Path relative to repo root."),
					"content": str("Full file content."),
				},
				"required": []string{"path", "content"},
			},
		},
		{
			Name: "git_status",
			Description: "Show worktree status. Requires at least one prior write_file call.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name: "git_diff",
			Description: "Show uncommitted changes in the worktree. Requires at least one prior write_file call.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": str("Limit diff to this file (optional).")},
			},
		},
		{
			Name: "git_commit",
			Description: "Stage all changes and commit them in the worktree. Requires at least one prior write_file call.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"message": str("Commit message.")},
				"required":   []string{"message"},
			},
		},
		{
			Name: "repo_status",
			Description: "Show backend state: ref, file count, working-set index, cache hits, GitHub rate limits, worktree provisioned. Cost: free. " +
				"Use when something seems off (rate limited? big repo?) or to verify session state.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name: "glimpse_help",
			Description: "Print the agent guide: when to use which tool, costs, and tips. Cost: free.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

const agentInstructions = `glimpse: read & search any github.com repo without cloning, then write through a lazy worktree.

Workflow:
  1. open_repo("https://github.com/owner/repo")  -> required first call
  2. find_files / list_directory                 -> free, in-memory navigation
  3. read_file                                   -> CDN fetch, ~100 ms cold, cached
  4. grep                                        -> Code Search + parallel fetch
  5. write_file / git_status / git_diff / git_commit -> first write triggers a one-time clone

Subtree pinning (use this on monorepos):
  open_repo("https://github.com/owner/repo/tree/<branch>/<path>")
  GitHub's Trees API truncates at ~7 MB / 100k entries. A truncated session still
  works for the entries that came back, but search and navigation are partial.
  Pinning to /tree/<branch>/<path> fetches only that subtree's tree; subsequent
  paths in find_files/list_directory/read_file/grep are relative to it.

Tips:
  - Grep with a literal substring (3+ chars) for full-repo coverage. Pure regex only hits files already in RAM.
  - read_file is keyed by blob SHA, so two paths with identical content share one cached copy.
  - repo_status surfaces rate-limit state and a 'truncated' flag if the tree was incomplete. If you hit a 403, set GITHUB_TOKEN.
  - glimpse_help prints this guide.
  - Only github.com URLs are supported. Other hosts return an error.`

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

func (s *server) dispatch(raw json.RawMessage) toolResult {
	var p callToolParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult("invalid params: " + err.Error())
	}

	if p.Name == "glimpse_help" {
		return textResult(agentInstructions)
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
			return errResult("url is required. Example: https://github.com/torvalds/linux")
		}
		return s.openRepo(args.URL, args.Ref)
	}

	s.mu.Lock()
	be := s.current
	s.mu.Unlock()
	if be == nil {
		return errResult("no repo open. Call open_repo(url) first. Example: open_repo(\"https://github.com/torvalds/linux\")")
	}

	var args struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Content string `json:"content"`
		Message string `json:"message"`
	}
	if len(p.Arguments) > 0 {
		_ = json.Unmarshal(p.Arguments, &args)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch p.Name {
	case "list_directory":
		return listDirectory(be, args.Path)
	case "find_files":
		return findFiles(be, args.Pattern, args.Path)
	case "file_info":
		return fileInfo(be, args.Path)
	case "read_file":
		return readFile(ctx, be, args.Path)
	case "grep":
		return grep(ctx, be, args.Pattern, args.Path)
	case "write_file":
		return writeFile(ctx, be, args.Path, args.Content)
	case "git_status":
		return gitStatus(ctx, be)
	case "git_diff":
		return gitDiff(ctx, be, args.Path)
	case "git_commit":
		return gitCommit(ctx, be, args.Message)
	case "repo_status":
		return repoStatus(be)
	default:
		return errResult("unknown tool: " + p.Name + ". Call glimpse_help for the tool list.")
	}
}

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

func (s *server) openRepo(rawURL, ref string) toolResult {
	parsed, err := gitbackend.ParseGitHubURL(rawURL)
	if err != nil {
		return errResult(err.Error() + ". Example: https://github.com/owner/repo")
	}
	if ref != "" {
		parsed.Ref = ref
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	be, err := gitbackend.Open(ctx, parsed, s.token, s.cacheDir)
	if err != nil {
		hint := ""
		if strings.Contains(err.Error(), "rate limit") {
			hint = " Set GITHUB_TOKEN to raise limits."
		}
		return errResult("open_repo failed: " + err.Error() + "." + hint)
	}

	s.mu.Lock()
	s.current = be
	s.mu.Unlock()

	stats := be.Stats()
	resp := map[string]any{
		"ok":         true,
		"owner":      be.Ref.Owner,
		"repo":       be.Ref.Repo,
		"ref":        be.Ref.Ref,
		"commit":     be.Ref.CommitSHA,
		"subtree":    stats.Subtree,
		"private":    stats.Private,
		"files":      stats.Files,
		"dirs":       stats.Dirs,
		"truncated":  stats.Truncated,
		"languages":  be.Languages(),
		"open_ms":    time.Since(start).Milliseconds(),
		"rate":       rateMap(stats.Rate),
		"next_steps": []string{"list_directory()", "find_files(\"*.go\")", "read_file(\"README.md\")", "grep(\"package main\")"},
	}
	if stats.Truncated {
		hint := "tree was truncated by the GitHub Trees API. " +
			"Re-open with /tree/<branch>/<path> to pin to a subtree."
		if stats.Subtree != "" {
			hint = "subtree " + stats.Subtree + " was itself truncated; pin deeper, e.g. /tree/<branch>/" + stats.Subtree + "/<subdir>."
		}
		resp["truncation_note"] = hint
	}
	return jsonResult(resp)
}

func listDirectory(be *gitbackend.Backend, p string) toolResult {
	p = gitbackend.NormalizePath(p)
	children := be.Children(p)
	if len(children) == 0 {
		// Could be a non-existent path or an empty directory. Distinguish.
		if _, ok := be.Lookup(p); !ok && p != "" {
			return errResult("not found: " + p + ". Try find_files to locate it.")
		}
	}
	type child struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size,omitempty"`
	}
	out := make([]child, 0, len(children))
	for _, e := range children {
		out = append(out, child{
			Name:  path.Base(e.Path),
			Path:  e.Path,
			IsDir: e.IsDir,
			Size:  e.Size,
		})
	}
	return jsonResult(map[string]any{
		"path":     p,
		"entries":  out,
		"cost":     "free",
	})
}

func findFiles(be *gitbackend.Backend, pattern, scope string) toolResult {
	if pattern == "" {
		return errResult("pattern is required. Examples: '*.go', 'src/**/*.ts', 'README'")
	}
	scope = gitbackend.NormalizePath(scope)

	tree := be.Tree()
	type hit struct {
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size,omitempty"`
	}
	var matches []hit
	for p, e := range tree {
		if scope != "" && !strings.HasPrefix(p, scope) {
			continue
		}
		if !globOrSubstrMatch(pattern, p) {
			continue
		}
		matches = append(matches, hit{Path: p, IsDir: e.IsDir, Size: e.Size})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Path < matches[j].Path })

	resp := map[string]any{
		"pattern": pattern,
		"matches": matches,
		"count":   len(matches),
		"cost":    "free",
	}
	if len(matches) == 0 {
		resp["next_steps"] = []string{"list_directory()", "find_files with a substring like 'auth' or '.go'"}
	}
	return jsonResult(resp)
}

func fileInfo(be *gitbackend.Backend, p string) toolResult {
	p = gitbackend.NormalizePath(p)
	e, ok := be.Lookup(p)
	if !ok {
		return errResult("not found: " + p + ". Try find_files to locate it.")
	}
	return jsonResult(map[string]any{
		"path":     e.Path,
		"is_dir":   e.IsDir,
		"size":     e.Size,
		"mode":     fmt.Sprintf("%o", e.Mode),
		"blob_sha": e.BlobSHA,
		"cost":     "free",
	})
}

func readFile(ctx context.Context, be *gitbackend.Backend, p string) toolResult {
	p = gitbackend.NormalizePath(p)
	if p == "" {
		return errResult("path is required.")
	}
	e, ok := be.Lookup(p)
	if !ok {
		return errResult("not found: " + p + ". Try find_files(\"" + path.Base(p) + "\") or list_directory.")
	}
	if e.IsDir {
		return errResult("path is a directory: " + p + ". Use list_directory.")
	}

	data, err := be.AccessFile(ctx, p)
	if err != nil {
		return errResult(err.Error())
	}

	cost := "ram_hit"
	stats := be.Stats()
	// Heuristic: if blob count went up since this read, it was a CDN fetch.
	// Not perfectly accurate under concurrency but good enough for an agent.
	if stats.CDNHits > 0 && stats.RAMHits == 0 {
		cost = "cdn_fetch"
	}
	return jsonResult(map[string]any{
		"path":    p,
		"size":    len(data),
		"content": string(data),
		"cost":    cost,
	})
}

func grep(ctx context.Context, be *gitbackend.Backend, pattern, scope string) toolResult {
	if pattern == "" {
		return errResult("pattern is required. Examples: 'handleAuth', 'func.*Login', 'TODO'")
	}
	res, err := be.Grep(ctx, pattern, scope)
	if err != nil {
		return errResult("grep: " + err.Error())
	}
	resp := map[string]any{
		"pattern":  pattern,
		"matches":  res.Matches,
		"count":    len(res.Matches),
		"searched": res.Search,
	}
	if res.Note != "" {
		resp["note"] = res.Note
	}
	return jsonResult(resp)
}

func writeFile(ctx context.Context, be *gitbackend.Backend, p, content string) toolResult {
	p = gitbackend.NormalizePath(p)
	if p == "" {
		return errResult("path is required.")
	}
	start := time.Now()
	wasWritable := be.WorktreeDir() != ""
	if err := be.WriteFile(ctx, p, content); err != nil {
		return errResult("write_file: " + err.Error())
	}
	resp := map[string]any{
		"path":          p,
		"size":          len(content),
		"worktree":      be.WorktreeDir(),
		"elapsed_ms":    time.Since(start).Milliseconds(),
		"first_write":   !wasWritable,
		"next_steps":    []string{"git_status", "git_diff", "git_commit(\"<message>\")"},
	}
	if !wasWritable {
		resp["note"] = "Lazy partial clone provisioned. Reads still prefer the CDN; only this file is materialized on disk."
	}
	return jsonResult(resp)
}

func gitStatus(ctx context.Context, be *gitbackend.Backend) toolResult {
	if be.WorktreeDir() == "" {
		return errResult("no worktree provisioned. Call write_file at least once first.")
	}
	out, err := be.RunGit(ctx, "status", "--short")
	if err != nil {
		return errResult("git status: " + err.Error() + ": " + string(out))
	}
	return textResult(strings.TrimRight(string(out), "\n"))
}

func gitDiff(ctx context.Context, be *gitbackend.Backend, scope string) toolResult {
	if be.WorktreeDir() == "" {
		return errResult("no worktree provisioned. Call write_file at least once first.")
	}
	args := []string{"diff", "--no-color"}
	if scope != "" {
		args = append(args, "--", scope)
	}
	out, err := be.RunGit(ctx, args...)
	if err != nil {
		return errResult("git diff: " + err.Error() + ": " + string(out))
	}
	return textResult(strings.TrimRight(string(out), "\n"))
}

func gitCommit(ctx context.Context, be *gitbackend.Backend, msg string) toolResult {
	if be.WorktreeDir() == "" {
		return errResult("no worktree provisioned. Call write_file at least once first.")
	}
	if msg == "" {
		return errResult("message is required.")
	}
	addOut, err := be.RunGit(ctx, "add", "-A")
	if err != nil {
		return errResult("git add: " + err.Error() + ": " + string(addOut))
	}
	commitOut, err := be.RunGit(ctx, "-c", "user.name=glimpse-agent", "-c", "user.email=agent@glimpse.local",
		"commit", "-m", msg)
	if err != nil {
		return errResult("git commit: " + err.Error() + ": " + string(commitOut))
	}
	return textResult(strings.TrimRight(string(commitOut), "\n"))
}

func repoStatus(be *gitbackend.Backend) toolResult {
	stats := be.Stats()
	resp := map[string]any{
		"ref": map[string]any{
			"owner":   be.Ref.Owner,
			"repo":    be.Ref.Repo,
			"ref":     be.Ref.Ref,
			"commit":  be.Ref.CommitSHA,
			"subtree": stats.Subtree,
		},
		"tree": map[string]any{
			"files":     stats.Files,
			"dirs":      stats.Dirs,
			"truncated": stats.Truncated,
		},
		"cache": map[string]any{
			"ram_hits":      stats.RAMHits,
			"disk_hits":     stats.DiskHits,
			"cdn_fetches":   stats.CDNHits,
			"bytes_fetched": stats.BytesFetched,
		},
		"index": map[string]any{
			"files":    stats.Index.Files,
			"bytes":    stats.Index.Bytes,
			"trigrams": stats.Index.Trigrams,
		},
		"writable":    stats.Writable,
		"worktree":    stats.WorktreeDir,
		"private":     stats.Private,
		"rate":        rateMap(stats.Rate),
	}
	return jsonResult(resp)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func rateMap(r gitbackend.RateSnapshot) map[string]any {
	return map[string]any{
		"search_remaining": r.SearchRemaining,
		"search_limit":     r.SearchLimit,
		"search_resets":    r.SearchReset.Unix(),
		"rest_remaining":   r.RestRemaining,
		"rest_limit":       r.RestLimit,
		"rest_resets":      r.RestReset.Unix(),
	}
}

func globOrSubstrMatch(pattern, p string) bool {
	if strings.ContainsAny(pattern, "*?[") {
		// Try /-aware matching first (matches against tail), then full path.
		base := path.Base(p)
		if ok, _ := filepath.Match(pattern, base); ok {
			return true
		}
		if ok, _ := filepath.Match(pattern, p); ok {
			return true
		}
		// Support "**" as a permissive wildcard by translating to a substring fragment.
		if strings.Contains(pattern, "**") {
			parts := strings.Split(pattern, "**")
			cursor := 0
			for i, part := range parts {
				if part == "" {
					continue
				}
				idx := strings.Index(p[cursor:], strings.TrimSuffix(strings.TrimPrefix(part, "/"), "/"))
				if idx < 0 {
					return false
				}
				cursor += idx + len(part)
				_ = i
			}
			return true
		}
		return false
	}
	return strings.Contains(p, pattern)
}

func textResult(s string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: s}}}
}

func errResult(s string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: s}}, IsError: true}
}

func jsonResult(v any) toolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errResult("internal: " + err.Error())
	}
	return toolResult{Content: []contentBlock{{Type: "text", Text: string(b)}}}
}

// ---------------------------------------------------------------------------
// MCP config printer
// ---------------------------------------------------------------------------

func printMCPConfig() {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"glimpse": map[string]any{
				"command": exePath(),
				"env": map[string]any{
					"GITHUB_TOKEN": "ghp_REPLACE_ME",
				},
			},
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	fmt.Println(string(b))
	fmt.Fprintln(os.Stderr, "\n# Paste into:")
	fmt.Fprintln(os.Stderr, "#   Cursor: ~/.cursor/mcp.json or <project>/.cursor/mcp.json")
	fmt.Fprintln(os.Stderr, "#   Claude Desktop: ~/Library/Application Support/Claude/claude_desktop_config.json")
	fmt.Fprintln(os.Stderr, "# GITHUB_TOKEN is optional; sets it raises rate limits and unlocks private repos.")
}

func exePath() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "glimpse-mcp"
}

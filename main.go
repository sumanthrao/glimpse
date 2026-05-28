// Command glimpse is a thin CLI for browsing github.com repos without cloning.
//
// All operations route through the gitbackend package, so glimpse and
// glimpse-mcp share one cache and one set of semantics. Only github.com URLs
// are accepted.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/surao/gitfs-accelerator/gitbackend"
)

var (
	flagSet  = flag.NewFlagSet("glimpse", flag.ExitOnError)
	cacheDir = flagSet.String("cache-dir", "", "Cache for partial clones (default: ~/.cache/glimpse)")
	tokenF   = flagSet.String("github-token", "", "GitHub token; defaults to $GITHUB_TOKEN")
	refF     = flagSet.String("ref", "", "Git ref (branch, tag, commit). Empty = default branch.")
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	args, cmd := parse(os.Args[1:])

	switch cmd {
	case "ls":
		runLs(args)
	case "cat":
		runCat(args)
	case "grep":
		runGrep(args)
	case "info":
		runInfo(args)
	case "find":
		runFind(args)
	case "serve":
		runServe()
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(1)
	}
}

// parse pulls global flags out of the command line, then returns the
// remaining positional args and the chosen subcommand.
func parse(in []string) ([]string, string) {
	// Find the subcommand: first non-flag argument that names a known command.
	commands := map[string]bool{
		"ls": true, "cat": true, "grep": true, "info": true, "find": true,
		"serve": true, "help": true, "--help": true, "-h": true,
	}
	var head []string
	cmd := ""
	rest := []string{}
	for i, a := range in {
		if commands[a] {
			cmd = a
			rest = in[i+1:]
			break
		}
		head = append(head, a)
	}
	if cmd == "" {
		// No subcommand; treat all as global flags + assume "ls".
		cmd = "ls"
		rest = nil
	}
	if err := flagSet.Parse(head); err != nil {
		os.Exit(2)
	}
	return rest, cmd
}

func resolveCacheDir() string {
	if *cacheDir != "" {
		return *cacheDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "glimpse")
}

func resolveToken() string {
	if *tokenF != "" {
		return *tokenF
	}
	return os.Getenv("GITHUB_TOKEN")
}

// openBackend opens the URL passed as args[0]. Returns the backend, the
// remaining args, and a function that closes (currently a no-op).
func openBackend(args []string) (*gitbackend.Backend, []string) {
	if len(args) == 0 {
		die("missing github.com URL. Example: glimpse ls https://github.com/torvalds/linux")
	}
	parsed, err := gitbackend.ParseGitHubURL(args[0])
	if err != nil {
		die("%v", err)
	}
	if *refF != "" {
		parsed.Ref = *refF
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	be, err := gitbackend.Open(ctx, parsed, resolveToken(), resolveCacheDir())
	if err != nil {
		die("open repo: %v", err)
	}
	return be, args[1:]
}

func runLs(args []string) {
	be, rest := openBackend(args)
	dir := ""
	if len(rest) > 0 {
		dir = rest[0]
	}
	dir = gitbackend.NormalizePath(dir)
	children := be.Children(dir)
	if len(children) == 0 {
		if _, ok := be.Lookup(dir); !ok && dir != "" {
			die("not found: %s", dir)
		}
		return
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Path < children[j].Path })
	for _, e := range children {
		base := filepath.Base(e.Path)
		if e.IsDir {
			fmt.Printf("  %s/\n", base)
		} else {
			fmt.Printf("  %-40s %d bytes\n", base, e.Size)
		}
	}
}

func runCat(args []string) {
	be, rest := openBackend(args)
	if len(rest) == 0 {
		die("usage: glimpse cat <url> <path>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	data, err := be.AccessFile(ctx, rest[0])
	if err != nil {
		die("%v", err)
	}
	_, _ = io.Copy(os.Stdout, strings.NewReader(string(data)))
}

func runGrep(args []string) {
	be, rest := openBackend(args)
	if len(rest) == 0 {
		die("usage: glimpse grep <url> <pattern> [path]")
	}
	pattern := rest[0]
	scope := ""
	if len(rest) > 1 {
		scope = rest[1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := be.Grep(ctx, pattern, scope)
	if err != nil {
		die("%v", err)
	}
	if res.Note != "" {
		fmt.Fprintln(os.Stderr, "note:", res.Note)
	}
	for _, m := range res.Matches {
		fmt.Printf("%s:%d:%s\n", m.Path, m.Line, m.Text)
	}
}

func runInfo(args []string) {
	be, rest := openBackend(args)
	if len(rest) == 0 {
		die("usage: glimpse info <url> <path>")
	}
	e, ok := be.Lookup(rest[0])
	if !ok {
		die("not found: %s", rest[0])
	}
	out := map[string]any{
		"path":     e.Path,
		"is_dir":   e.IsDir,
		"size":     e.Size,
		"mode":     fmt.Sprintf("%o", e.Mode),
		"blob_sha": e.BlobSHA,
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

func runFind(args []string) {
	be, rest := openBackend(args)
	if len(rest) == 0 {
		die("usage: glimpse find <url> <pattern> [scope]")
	}
	pattern := rest[0]
	scope := ""
	if len(rest) > 1 {
		scope = rest[1]
	}
	scope = gitbackend.NormalizePath(scope)
	for p, e := range be.Tree() {
		if scope != "" && !strings.HasPrefix(p, scope) {
			continue
		}
		if !strings.Contains(p, pattern) {
			ok, _ := filepath.Match(pattern, filepath.Base(p))
			if !ok {
				continue
			}
		}
		if e.IsDir {
			fmt.Printf("dir  %s\n", p)
		} else {
			fmt.Printf("file %s (%d bytes)\n", p, e.Size)
		}
	}
}

func runServe() {
	mcp, err := exec.LookPath("glimpse-mcp")
	if err != nil {
		self, _ := os.Executable()
		mcp = filepath.Join(filepath.Dir(self), "glimpse-mcp")
		if _, err := os.Stat(mcp); err != nil {
			die("glimpse-mcp not found. Build it:\n  go build -o glimpse-mcp ./cmd/glimpse-mcp")
		}
	}
	cmd := exec.Command(mcp)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `glimpse — github.com repos without cloning.

Usage:
  glimpse [flags] <command> <url> [args...]

Commands:
  ls   <url> [path]            List directory entries
  cat  <url> <path>            Print file contents (CDN fetch, cached)
  grep <url> <pattern> [path]  Search file contents (Code Search + CDN)
  info <url> <path>            Show file metadata
  find <url> <pattern> [path]  Find paths by substring or glob
  serve                        Start the MCP server (for AI agents)

Flags:
  --ref <ref>                  Branch, tag, or commit. Default: repo default branch.
  --cache-dir <dir>            Cache for lazy clones. Default: ~/.cache/glimpse.
  --github-token <token>       Auth token. Defaults to $GITHUB_TOKEN.

Examples:
  glimpse ls https://github.com/torvalds/linux
  glimpse cat https://github.com/torvalds/linux README
  glimpse grep https://github.com/torvalds/linux 'EXPORT_SYMBOL_GPL'
  glimpse serve   # start MCP server`)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

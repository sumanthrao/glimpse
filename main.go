// glimpse: fast git repo browsing from the command line.
// No FUSE, no system dependencies — just Go and the git CLI.
//
// Usage:
//
//	glimpse <url-or-path> [command] [args...]
//	glimpse ls [path]
//	glimpse cat <file>
//	glimpse grep <pattern> [path]
//	glimpse serve                   # start MCP server
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	args := os.Args[1:]

	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	// Parse: first arg might be a URL/path (repo target) or a subcommand
	repo, cmd, cmdArgs := parseArgs(args)

	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".cache", "glimpse")

	repoDir, err := resolveRepo(repo, cacheDir)
	if err != nil {
		die("%v", err)
	}

	ref := "HEAD"

	switch cmd {
	case "ls":
		path := ""
		if len(cmdArgs) > 0 {
			path = cmdArgs[0]
		}
		lsTree(repoDir, ref, path)

	case "cat":
		if len(cmdArgs) == 0 {
			die("usage: glimpse cat <file>")
		}
		catFile(repoDir, ref, cmdArgs[0])

	case "grep":
		if len(cmdArgs) == 0 {
			die("usage: glimpse grep <pattern> [path]")
		}
		pattern := cmdArgs[0]
		path := ""
		if len(cmdArgs) > 1 {
			path = cmdArgs[1]
		}
		grepRepo(repoDir, ref, pattern, path)

	case "serve":
		serveMCP(repoDir)

	default:
		// If no recognized command, treat it as "ls" on the repo root
		lsTree(repoDir, ref, "")
	}
}

func parseArgs(args []string) (repo, cmd string, cmdArgs []string) {
	commands := map[string]bool{"ls": true, "cat": true, "grep": true, "serve": true}

	// If first arg is a known command, infer repo from cwd
	if commands[args[0]] {
		return "", args[0], args[1:]
	}

	// First arg is the repo
	repo = args[0]
	if len(args) > 1 && commands[args[1]] {
		return repo, args[1], args[2:]
	}

	// No command — default to "ls"
	return repo, "ls", args[1:]
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

func lsTree(repoDir, ref, path string) {
	args := []string{"-C", repoDir, "ls-tree", "-l", ref}
	if path != "" {
		path = strings.TrimSuffix(path, "/")
		args = append(args, "--", path+"/")
	}

	out, err := exec.Command("git", args...).Output()
	if err != nil {
		die("not found: %s", path)
	}

	prefix := ""
	if path != "" {
		prefix = path + "/"
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
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
			fmt.Printf("  %s/\n", name)
		} else {
			fmt.Printf("  %-40s %s bytes\n", name, meta[3])
		}
	}
}

func catFile(repoDir, ref, path string) {
	out, err := exec.Command("git", "-C", repoDir, "show", ref+":"+path).Output()
	if err != nil {
		die("not found: %s", path)
	}
	os.Stdout.Write(out)
}

func grepRepo(repoDir, ref, pattern, path string) {
	args := []string{"-C", repoDir, "grep", "-n", "-I", pattern, ref}
	if path != "" {
		args = append(args, "--", path)
	}

	out, err := exec.Command("git", args...).Output()
	if err != nil {
		fmt.Println("no matches found")
		return
	}

	prefix := ref + ":"
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		fmt.Println(strings.TrimPrefix(line, prefix))
	}
}

func serveMCP(repoDir string) {
	// Exec the MCP server binary if available, otherwise tell the user to build it
	mcpBin, err := exec.LookPath("glimpse-mcp")
	if err != nil {
		// Try next to our own binary
		self, _ := os.Executable()
		mcpBin = filepath.Join(filepath.Dir(self), "glimpse-mcp")
		if _, err := os.Stat(mcpBin); err != nil {
			die("glimpse-mcp not found. Build it:\n  go build -o glimpse-mcp ./cmd/glimpse-mcp")
		}
	}

	cmd := exec.Command(mcpBin, "--repo", repoDir)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Repo resolution (clone remote URLs, use local paths directly)
// ---------------------------------------------------------------------------

func resolveRepo(repoOrURL, cacheDir string) (string, error) {
	if repoOrURL == "" {
		return findGitRoot()
	}

	if isGitURL(repoOrURL) {
		return cloneOrFetch(repoOrURL, cacheDir)
	}

	abs, err := filepath.Abs(repoOrURL)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	// Check for normal repo (.git dir) or bare repo (HEAD file)
	if fileExists(filepath.Join(abs, ".git")) || fileExists(filepath.Join(abs, "HEAD")) {
		return abs, nil
	}
	return "", fmt.Errorf("not a git repository: %s", abs)
}

func cloneOrFetch(url, cacheDir string) (string, error) {
	dir := repoCacheDir(cacheDir, url)

	if fileExists(filepath.Join(dir, "HEAD")) {
		fmt.Fprintf(os.Stderr, "cached: %s\n", dir)
		cmd := exec.Command("git", "-C", dir, "fetch", "--quiet", "origin")
		cmd.Stderr = os.Stderr
		_ = cmd.Run()
		return dir, nil
	}

	fmt.Fprintf(os.Stderr, "cloning %s ...\n", url)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("git", "clone", "--bare", "--quiet", url, dir)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git clone --bare: %w", err)
	}
	fmt.Fprintf(os.Stderr, "ready: %s\n", dir)
	return dir, nil
}

func repoCacheDir(cacheDir, url string) string {
	name := strings.TrimSuffix(url, ".git")
	r := strings.NewReplacer("://", "-", "/", "-", ":", "-", "@", "-")
	return filepath.Join(cacheDir, r.Replace(name))
}

func isGitURL(s string) bool {
	return strings.Contains(s, "://") || strings.HasPrefix(s, "git@")
}

func findGitRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if fileExists(filepath.Join(dir, ".git")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not inside a git repo — pass a URL or path:\n  glimpse https://github.com/org/repo.git\n  glimpse /path/to/local/repo")
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func usage() {
	fmt.Fprintf(os.Stderr, `glimpse — fast git repo browsing. No checkout needed.

Usage:
  glimpse <url-or-path> [command] [args...]

Commands:
  ls   [path]            List files and directories
  cat  <file>            Print file contents
  grep <pattern> [path]  Search file contents
  serve                  Start MCP server (for AI agents)

Examples:
  glimpse https://github.com/org/repo.git          # clone + list root
  glimpse https://github.com/org/repo.git ls src/   # list src/
  glimpse https://github.com/org/repo.git cat README.md
  glimpse https://github.com/org/repo.git grep "func main"
  glimpse cat src/main.go                           # if inside a repo
  glimpse serve                                     # start MCP server
`)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/surao/gitfs-accelerator/fusefs"
	"github.com/surao/gitfs-accelerator/gitbackend"
)

func main() {
	repoPath := flag.String("repo", "", "Git repository — local path or remote URL (https/ssh)")
	ref := flag.String("ref", "HEAD", "Git ref to mount (branch, tag, or commit hash)")
	mountPoint := flag.String("mount", "", "Mount point directory (defaults to repo worktree or ./<repo-name>)")
	cacheDir := flag.String("cache-dir", "", "Where to store clones of remote repos (default: ~/.cache/glimpse)")
	debug := flag.Bool("debug", false, "Enable FUSE debug logging")
	ephemeral := flag.Bool("ephemeral", false, "Ephemeral mode: skip sparse-checkout setup")
	flag.Parse()

	// Support positional arg: glimpse https://github.com/org/repo.git
	if *repoPath == "" && flag.NArg() > 0 {
		*repoPath = flag.Arg(0)
	}

	if *cacheDir == "" {
		home, _ := os.UserHomeDir()
		*cacheDir = filepath.Join(home, ".cache", "glimpse")
	}

	if *repoPath == "" {
		detected, err := findGitRoot()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: not inside a git repository and no repo specified\n")
			fmt.Fprintf(os.Stderr, "\nUsage: glimpse [<url-or-path>] [--mount <mountpoint>] [--ref <ref>]\n\n")
			fmt.Fprintf(os.Stderr, "Mount any git repo — local path or remote URL.\n")
			fmt.Fprintf(os.Stderr, "Files are served from the git object store in memory.\n\n")
			fmt.Fprintf(os.Stderr, "Examples:\n")
			fmt.Fprintf(os.Stderr, "  glimpse                                        # mount cwd repo\n")
			fmt.Fprintf(os.Stderr, "  glimpse https://github.com/org/repo.git         # clone + mount\n")
			fmt.Fprintf(os.Stderr, "  glimpse git@github.com:org/repo.git             # clone + mount\n")
			fmt.Fprintf(os.Stderr, "  glimpse --repo /path/to/local/repo              # mount local repo\n\n")
			fmt.Fprintf(os.Stderr, "Options:\n")
			flag.PrintDefaults()
			os.Exit(1)
		}
		*repoPath = detected
	}

	// Resolve: if it's a URL, clone it; if local, use directly
	absRepo, gitDir, cloned, err := resolveRepo(*repoPath, *cacheDir)
	if err != nil {
		log.Fatalf("%v", err)
	}

	if cloned {
		// Remote clone — force ephemeral (bare repo, no sparse-checkout)
		*ephemeral = true
	}

	// Determine mount point
	if *mountPoint == "" {
		if cloned {
			// For remote repos, mount in cwd under the repo name
			*mountPoint = repoNameFromURL(*repoPath)
		} else {
			*mountPoint = absRepo
		}
	}
	absMount, err := filepath.Abs(*mountPoint)
	if err != nil {
		log.Fatalf("resolve mount path: %v", err)
	}

	if cloned {
		if err := os.MkdirAll(absMount, 0o755); err != nil {
			log.Fatalf("create mount point: %v", err)
		}
	}

	if !hasFUSE() {
		fmt.Fprintf(os.Stderr, "error: FUSE is not available on this system.\n\n")
		fmt.Fprintf(os.Stderr, "The glimpse FUSE mount requires:\n")
		fmt.Fprintf(os.Stderr, "  macOS:  brew install --cask macfuse  (reboot after install)\n")
		fmt.Fprintf(os.Stderr, "  Linux:  sudo apt install fuse3 libfuse3-dev\n\n")
		fmt.Fprintf(os.Stderr, "Alternative: use the MCP server (no FUSE needed):\n")
		fmt.Fprintf(os.Stderr, "  go build -o glimpse-mcp ./cmd/glimpse-mcp\n")
		fmt.Fprintf(os.Stderr, "  glimpse-mcp --repo %s\n\n", absRepo)
		fmt.Fprintf(os.Stderr, "The MCP server provides the same read/grep/write tools\n")
		fmt.Fprintf(os.Stderr, "over the Model Context Protocol — works anywhere Go and git are installed.\n")
		os.Exit(1)
	}

	if !*ephemeral {
		if err := ensureSparseCheckout(absRepo); err != nil {
			log.Fatalf("initialize sparse-checkout: %v", err)
		}
	}

	backend, err := gitbackend.Open(absRepo)
	if err != nil {
		log.Fatalf("open git repo: %v", err)
	}

	commitHash, err := backend.ResolveRef(*ref)
	if err != nil {
		log.Fatalf("resolve ref %q: %v", *ref, err)
	}

	treeHash, err := backend.RootTree(commitHash)
	if err != nil {
		log.Fatalf("get root tree: %v", err)
	}

	root := fusefs.NewGitFS(backend, treeHash, absMount, gitDir)

	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName: "glimpse",
			Name:   "glimpse",
			Debug:  *debug,
		},
	}

	server, err := fs.Mount(absMount, root, opts)
	if err != nil {
		log.Fatalf("mount: %v", err)
	}

	fmt.Printf("glimpse mounted\n")
	if cloned {
		fmt.Printf("  source: %s\n", *repoPath)
		fmt.Printf("  clone:  %s\n", absRepo)
	} else {
		fmt.Printf("  repo:   %s\n", absRepo)
	}
	fmt.Printf("  ref:    %s (%s)\n", *ref, commitHash.String()[:12])
	fmt.Printf("  mount:  %s\n", absMount)
	if *ephemeral || cloned {
		fmt.Printf("  mode:   ephemeral (all reads from memory)\n")
	} else {
		fmt.Printf("  mode:   hybrid (reads in memory, writes materialize to disk)\n")
	}
	fmt.Printf("\nPress Ctrl+C to unmount.\n")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		fmt.Printf("\nUnmounting...\n")
		server.Unmount()
	}()

	server.Wait()

	stats := root.Stats()
	fmt.Printf("\nSession stats:\n")
	fmt.Printf("  Blobs fetched:             %d\n", stats.BlobsRead)
	fmt.Printf("  Bytes fetched:             %d\n", stats.BytesRead)
	fmt.Printf("  Served from memory:        %d\n", stats.BlobsRead-stats.DiskMaterializations)
	fmt.Printf("  Materialized to disk:      %d\n", stats.DiskMaterializations)
	fmt.Printf("  Directories listed:        %d\n", stats.DirReads)
}

// resolveRepo takes a local path or remote URL and returns:
//
//	(repoDir, gitDir, cloned, error)
//
// For URLs, it clones (or reuses a cached clone) into cacheDir.
func resolveRepo(repoOrURL, cacheDir string) (string, string, bool, error) {
	if isGitURL(repoOrURL) {
		dir, err := cloneOrFetch(repoOrURL, cacheDir)
		if err != nil {
			return "", "", false, err
		}
		// bare clone: the repo IS the git dir
		return dir, dir, true, nil
	}

	abs, err := filepath.Abs(repoOrURL)
	if err != nil {
		return "", "", false, fmt.Errorf("resolve path: %w", err)
	}

	gitDir := filepath.Join(abs, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		return "", "", false, fmt.Errorf("not a git repository: %s", abs)
	}

	return abs, gitDir, false, nil
}

func cloneOrFetch(url, cacheDir string) (string, error) {
	dir := repoCacheDir(cacheDir, url)

	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		fmt.Printf("cached clone: %s\n", dir)
		cmd := exec.Command("git", "-C", dir, "fetch", "--quiet", "origin")
		cmd.Stderr = os.Stderr
		_ = cmd.Run()
		return dir, nil
	}

	fmt.Printf("cloning %s ...\n", url)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("git", "clone", "--bare", "--quiet", url, dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git clone --bare: %w", err)
	}
	fmt.Printf("cloned to %s\n", dir)
	return dir, nil
}

func repoCacheDir(cacheDir, url string) string {
	name := url
	name = strings.TrimSuffix(name, ".git")
	r := strings.NewReplacer("://", "-", "/", "-", ":", "-", "@", "-")
	name = r.Replace(name)
	return filepath.Join(cacheDir, name)
}

func repoNameFromURL(url string) string {
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimSuffix(url, "/")
	if i := strings.LastIndex(url, "/"); i >= 0 {
		return url[i+1:]
	}
	if i := strings.LastIndex(url, ":"); i >= 0 {
		return url[i+1:]
	}
	return url
}

func isGitURL(s string) bool {
	return strings.Contains(s, "://") || strings.HasPrefix(s, "git@")
}

// hasFUSE checks whether FUSE is available on this system.
func hasFUSE() bool {
	switch {
	case fileExists("/Library/Filesystems/macfuse.fs") || fileExists("/usr/local/lib/libosxfuse.dylib"):
		return true // macFUSE
	case fileExists("/dev/fuse"):
		return true // Linux FUSE
	default:
		return false
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// findGitRoot walks up from the cwd to find the nearest .git directory.
func findGitRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not a git repository")
		}
		dir = parent
	}
}

// ensureSparseCheckout initializes git sparse-checkout in no-cone mode
// if it's not already configured.
func ensureSparseCheckout(repoPath string) error {
	scPath := filepath.Join(repoPath, ".git", "info", "sparse-checkout")
	if _, err := os.Stat(scPath); err == nil {
		return nil
	}

	cmd := exec.Command("git", "sparse-checkout", "init", "--no-cone")
	cmd.Dir = repoPath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git sparse-checkout init: %w\n%s", err, out)
	}

	return nil
}

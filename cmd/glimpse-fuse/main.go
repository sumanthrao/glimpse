// Command glimpse-fuse mounts a github.com repository as a FUSE filesystem.
//
// All reads stream lazily from raw.githubusercontent.com (cached in RAM by
// blob SHA). Writes lazily provision a partial bare clone + sparse worktree
// under the cache dir.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/surao/gitfs-accelerator/fusefs"
	"github.com/surao/gitfs-accelerator/gitbackend"
)

func main() {
	repoURL := flag.String("repo", "", "github.com URL (https or ssh)")
	ref := flag.String("ref", "", "Git ref (branch, tag, commit). Empty = default branch.")
	mountPoint := flag.String("mount", "", "Mount point directory (default: ./<repo-name>)")
	cacheDir := flag.String("cache-dir", "", "Cache for partial clones (default: ~/.cache/glimpse)")
	token := flag.String("github-token", "", "GitHub token; defaults to $GITHUB_TOKEN")
	debug := flag.Bool("debug", false, "Enable FUSE debug logging")
	flag.Parse()

	if *repoURL == "" && flag.NArg() > 0 {
		*repoURL = flag.Arg(0)
	}
	if *repoURL == "" {
		fmt.Fprintln(os.Stderr, "error: missing github.com URL")
		fmt.Fprintln(os.Stderr, "\nUsage: glimpse-fuse <https://github.com/owner/repo> [--mount <dir>] [--ref <ref>]")
		os.Exit(1)
	}

	parsed, err := gitbackend.ParseGitHubURL(*repoURL)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if *ref != "" {
		parsed.Ref = *ref
	}

	if *cacheDir == "" {
		home, _ := os.UserHomeDir()
		*cacheDir = filepath.Join(home, ".cache", "glimpse")
	}
	if *token == "" {
		*token = os.Getenv("GITHUB_TOKEN")
	}

	if *mountPoint == "" {
		*mountPoint = parsed.Repo
	}
	absMount, err := filepath.Abs(*mountPoint)
	if err != nil {
		log.Fatalf("resolve mount path: %v", err)
	}
	if err := os.MkdirAll(absMount, 0o755); err != nil {
		log.Fatalf("create mount point: %v", err)
	}

	if !hasFUSE() {
		fmt.Fprintln(os.Stderr, "error: FUSE not available on this system.")
		fmt.Fprintln(os.Stderr, "Install macFUSE (macOS) or fuse3 (Linux), or use the MCP server (glimpse-mcp).")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	backend, err := gitbackend.Open(ctx, parsed, *token, *cacheDir)
	if err != nil {
		log.Fatalf("open repo: %v", err)
	}

	root := fusefs.NewGitFS(backend)
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
	fmt.Printf("  repo:  %s\n", backend.Ref)
	fmt.Printf("  mount: %s\n", absMount)
	fmt.Printf("  cache: %s\n", *cacheDir)
	fmt.Printf("  files: %d (%d dirs)\n", backend.Stats().Files, backend.Stats().Dirs)
	fmt.Printf("\nReads stream lazily from raw.githubusercontent.com.\nWrites trigger a one-time partial clone + worktree.\nPress Ctrl+C to unmount.\n")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nUnmounting...")
		_ = server.Unmount()
	}()

	server.Wait()

	stats := backend.Stats()
	fmt.Printf("\nSession stats:\n")
	fmt.Printf("  CDN fetches:    %d\n", stats.CDNHits)
	fmt.Printf("  RAM hits:       %d\n", stats.RAMHits)
	fmt.Printf("  Disk hits:      %d\n", stats.DiskHits)
	fmt.Printf("  Bytes fetched:  %d\n", stats.BytesFetched)
	fmt.Printf("  Index size:     %d files / %d trigrams\n", stats.Index.Files, stats.Index.Trigrams)
}

func hasFUSE() bool {
	switch {
	case fileExists("/dev/fuse"):
		return true
	case fileExists("/Library/Filesystems/macfuse.fs"):
		return true
	case fileExists("/usr/local/lib/libosxfuse.dylib"):
		return true
	default:
		return false
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

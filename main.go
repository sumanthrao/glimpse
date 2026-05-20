package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/surao/gitfs-accelerator/fusefs"
	"github.com/surao/gitfs-accelerator/gitbackend"
)

func main() {
	repoPath := flag.String("repo", "", "Path to git repository (auto-detected from cwd if omitted)")
	ref := flag.String("ref", "HEAD", "Git ref to mount (branch, tag, or commit hash)")
	mountPoint := flag.String("mount", "", "Mount point directory (defaults to repo worktree)")
	debug := flag.Bool("debug", false, "Enable FUSE debug logging")
	ephemeral := flag.Bool("ephemeral", false, "Ephemeral mode: skip sparse-checkout setup (reads stay in memory, nothing touches disk unless written)")
	flag.Parse()

	if *repoPath == "" {
		detected, err := findGitRoot()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: not inside a git repository and --repo not specified\n")
			fmt.Fprintf(os.Stderr, "\nUsage: glimpse [--repo <path>] [--mount <mountpoint>] [--ref <ref>]\n\n")
			fmt.Fprintf(os.Stderr, "Gaze into a git repo — lazy file access, entirely in memory.\n")
			fmt.Fprintf(os.Stderr, "Files are only fetched from the git object store when you read them.\n\n")
			fmt.Fprintf(os.Stderr, "Options:\n")
			flag.PrintDefaults()
			os.Exit(1)
		}
		*repoPath = detected
	}

	absRepo, err := filepath.Abs(*repoPath)
	if err != nil {
		log.Fatalf("resolve repo path: %v", err)
	}

	gitDir := filepath.Join(absRepo, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		log.Fatalf("not a git repository: %s", absRepo)
	}

	if *mountPoint == "" {
		*mountPoint = absRepo
	}
	absMount, err := filepath.Abs(*mountPoint)
	if err != nil {
		log.Fatalf("resolve mount path: %v", err)
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

	fmt.Printf("glimpse mounted (lazy sparse checkout)\n")
	fmt.Printf("  repo:   %s\n", absRepo)
	fmt.Printf("  ref:    %s (%s)\n", *ref, commitHash.String()[:12])
	fmt.Printf("  mount:  %s\n", absMount)
	if *ephemeral {
		fmt.Printf("  mode:   ephemeral (reads stay in memory, no sparse-checkout)\n")
	} else {
		fmt.Printf("  mode:   hybrid (reads in memory, writes materialize to disk)\n")
	}
	fmt.Printf("\nReads are served from memory. Writes trigger disk materialization.\n")
	fmt.Printf("Press Ctrl+C to unmount.\n")

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

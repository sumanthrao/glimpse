package gitbackend

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// EnsureWritable provisions a partial bare clone (--filter=blob:none) plus a
// sparse worktree the first time it's called. Subsequent calls return the
// existing worktree path. The CDN read path stays the default; the worktree
// only serves files that have been materialized via WriteFile.
//
// Returns (worktreeDir, error). On error subsequent calls retry — we don't
// poison the once.
func (b *Backend) EnsureWritable(ctx context.Context) (string, error) {
	b.cloneOnce.Do(func() {
		b.cloneErr = b.provisionWorktree(ctx)
	})
	if b.cloneErr != nil {
		// Reset so a future call can retry. cloneOnce only fires the first
		// successful call's branch; for failures we want a chance to retry.
		b.cloneOnce = sync.Once{}
		return "", b.cloneErr
	}
	return b.wtDir, nil
}

func (b *Backend) provisionWorktree(ctx context.Context) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git CLI not found on PATH; required for write operations")
	}

	id := repoCacheID(b.Ref)
	bareDir := filepath.Join(b.cacheDir, id+".git")
	wtDir := filepath.Join(b.cacheDir, id+"-wt")

	if _, err := os.Stat(filepath.Join(bareDir, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(bareDir), 0o755); err != nil {
			return fmt.Errorf("create cache dir: %w", err)
		}
		args := []string{
			"clone", "--bare", "--filter=blob:none", "--quiet",
			fmt.Sprintf("https://github.com/%s/%s.git", b.Ref.Owner, b.Ref.Repo),
			bareDir,
		}
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Stderr = os.Stderr
		if b.gh.token != "" {
			// Configure auth via X-Access-Token header so token isn't in argv.
			cmd.Env = append(os.Environ(),
				"GIT_HTTP_EXTRAHEADER=Authorization: Bearer "+b.gh.token,
			)
		}
		if err := cmd.Run(); err != nil {
			_ = os.RemoveAll(bareDir)
			return fmt.Errorf("git clone --bare --filter=blob:none: %w", err)
		}
	}

	if _, err := os.Stat(filepath.Join(wtDir, ".git")); err != nil {
		args := []string{"-C", bareDir, "worktree", "add", "--detach", "--no-checkout", wtDir, b.Ref.CommitSHA}
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git worktree add: %w", err)
		}
		if err := ensureSparseCheckout(wtDir); err != nil {
			return fmt.Errorf("init sparse-checkout: %w", err)
		}
	}

	b.bareDir = bareDir
	b.wtDir = wtDir
	return nil
}

// WriteFile materializes a file under the lazy worktree. The first call
// triggers EnsureWritable; subsequent calls just write.
func (b *Backend) WriteFile(ctx context.Context, p, content string) error {
	p = NormalizePath(p)
	if p == "" {
		return fmt.Errorf("path is required")
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("path must not contain '..'")
	}
	wt, err := b.EnsureWritable(ctx)
	if err != nil {
		return err
	}
	full := filepath.Join(wt, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := UpdateSparseCheckout(filepath.Join(wt, ".git"), p); err != nil {
		return fmt.Errorf("sparse-checkout: %w", err)
	}
	// Invalidate any cached blob bytes for this path's SHA so subsequent reads
	// pick up the fresh content from disk. Safest is to drop the SHA→bytes
	// entry; the path stays in the tree map.
	if e, ok := b.tree[p]; ok && e.BlobSHA != "" {
		b.cache.Delete(e.BlobSHA)
	}
	return nil
}

// ensureSparseCheckout sets up sparse-checkout in a worktree without using
// git sparse-checkout init (which adds extensions.worktreeConfig).
func ensureSparseCheckout(wtDir string) error {
	scPath := filepath.Join(wtDir, ".git", "info", "sparse-checkout")
	if _, err := os.Stat(scPath); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(scPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(scPath, []byte("\n"), 0o644); err != nil {
		return err
	}
	cmd := exec.Command("git", "config", "--local", "core.sparseCheckout", "true")
	cmd.Dir = wtDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("enable sparse-checkout: %w\n%s", err, out)
	}
	return nil
}

var sparseMu sync.Mutex

// UpdateSparseCheckout appends a path to .git/info/sparse-checkout so the
// materialized file shows up in `git status` / `git diff` / `git commit`.
func UpdateSparseCheckout(gitDir, relPath string) error {
	sparseMu.Lock()
	defer sparseMu.Unlock()

	scPath := filepath.Join(gitDir, "info", "sparse-checkout")
	existing, err := readLines(scPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	normalized := "/" + relPath
	for _, line := range existing {
		if line == normalized {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(scPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(scPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, normalized)
	return err
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

// repoCacheID is a filesystem-safe identifier for an (owner, repo) pair.
func repoCacheID(ref RepoRef) string {
	return fmt.Sprintf("github.com-%s-%s", ref.Owner, ref.Repo)
}

// GitDir returns the worktree's .git directory path. Empty if not provisioned.
func (b *Backend) GitDir() string {
	if b.wtDir == "" {
		return ""
	}
	return filepath.Join(b.wtDir, ".git")
}

// WorktreeDir returns the lazy worktree root. Empty if not provisioned.
func (b *Backend) WorktreeDir() string { return b.wtDir }

// BareDir returns the partial bare clone path. Empty if not provisioned.
func (b *Backend) BareDir() string { return b.bareDir }

// RunGit runs a git command inside the worktree. Returns combined stdout.
// Returns an error if the worktree hasn't been provisioned.
func (b *Backend) RunGit(ctx context.Context, args ...string) ([]byte, error) {
	if b.wtDir == "" {
		return nil, fmt.Errorf("worktree not provisioned; call write_file first or run a tool that requires writes")
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", b.wtDir}, args...)...)
	return cmd.CombinedOutput()
}

package gitbackend

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestAPIWriteEndToEnd exercises the full diskless write flow against a real
// GitHub repository. It creates a test branch, writes a file, commits, pushes,
// verifies the commit, then cleans up the branch.
//
// Requires: GITHUB_TOKEN with repo write access.
// Set GLIMPSE_TEST_REPO to "owner/repo" (default: skips).
func TestAPIWriteEndToEnd(t *testing.T) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Skip("GITHUB_TOKEN not set; skipping integration test")
	}
	testRepo := os.Getenv("GLIMPSE_TEST_REPO")
	if testRepo == "" {
		t.Skip("GLIMPSE_TEST_REPO not set (should be owner/repo); skipping")
	}
	parts := strings.SplitN(testRepo, "/", 2)
	if len(parts) != 2 {
		t.Fatalf("GLIMPSE_TEST_REPO must be owner/repo, got %q", testRepo)
	}
	owner, repo := parts[0], parts[1]

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ref := RepoRef{Owner: owner, Repo: repo}
	be, err := Open(ctx, ref, token, t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	branch := fmt.Sprintf("glimpse-test-%d", time.Now().UnixNano())
	testFile := "glimpse-api-test.txt"
	testContent := fmt.Sprintf("Written by glimpse API write test at %s\n", time.Now().Format(time.RFC3339))

	if err := be.WriteFileAPI(testFile, []byte(testContent)); err != nil {
		t.Fatalf("WriteFileAPI: %v", err)
	}

	pending := be.PendingFiles()
	if len(pending) != 1 || pending[0] != testFile {
		t.Fatalf("PendingFiles: expected [%s], got %v", testFile, pending)
	}

	result, err := be.CreateBranchAndPush(ctx, "test: glimpse API write e2e", branch)
	if err != nil {
		t.Fatalf("CreateBranchAndPush: %v", err)
	}
	t.Logf("pushed commit %s to branch %s", result.CommitSHA, result.Branch)

	if result.CommitSHA == "" {
		t.Fatal("expected non-empty commit SHA")
	}
	if result.Branch != branch {
		t.Errorf("expected branch %s, got %s", branch, result.Branch)
	}

	// Verify the commit exists by fetching it.
	gh := newGitHubClient(token)
	var commit commitInfo
	err = gh.doJSON(ctx, fmt.Sprintf("/repos/%s/%s/git/commits/%s", owner, repo, result.CommitSHA), &commit)
	if err != nil {
		t.Fatalf("verify commit: %v", err)
	}
	if commit.SHA != result.CommitSHA {
		t.Errorf("fetched commit SHA mismatch: %s vs %s", commit.SHA, result.CommitSHA)
	}

	// Verify file content via raw CDN.
	data, err := gh.fetchRawBlob(ctx, owner, repo, result.CommitSHA, testFile)
	if err != nil {
		t.Fatalf("verify file content: %v", err)
	}
	if string(data) != testContent {
		t.Errorf("file content mismatch:\n  got:  %q\n  want: %q", string(data), testContent)
	}

	// Verify pending list is now empty.
	if p := be.PendingFiles(); len(p) != 0 {
		t.Errorf("PendingFiles after push should be empty, got %v", p)
	}

	// Cleanup: delete the test branch.
	err = gh.deleteRef(ctx, owner, repo, "heads/"+branch)
	if err != nil {
		t.Logf("warning: failed to delete test branch %s: %v (manual cleanup needed)", branch, err)
	} else {
		t.Logf("cleaned up branch %s", branch)
	}
}

// TestAPIWriteMultiFile stages multiple files, commits, and verifies all are present.
func TestAPIWriteMultiFile(t *testing.T) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Skip("GITHUB_TOKEN not set; skipping integration test")
	}
	testRepo := os.Getenv("GLIMPSE_TEST_REPO")
	if testRepo == "" {
		t.Skip("GLIMPSE_TEST_REPO not set; skipping")
	}
	parts := strings.SplitN(testRepo, "/", 2)
	owner, repo := parts[0], parts[1]

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ref := RepoRef{Owner: owner, Repo: repo}
	be, err := Open(ctx, ref, token, t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	branch := fmt.Sprintf("glimpse-multifile-test-%d", time.Now().UnixNano())
	files := map[string]string{
		"test/a.txt": "content A\n",
		"test/b.txt": "content B\n",
		"test/c.txt": "content C\n",
	}

	for path, content := range files {
		if err := be.WriteFileAPI(path, []byte(content)); err != nil {
			t.Fatalf("WriteFileAPI(%s): %v", path, err)
		}
	}

	if p := be.PendingFiles(); len(p) != 3 {
		t.Fatalf("expected 3 pending files, got %d", len(p))
	}

	result, err := be.CreateBranchAndPush(ctx, "test: multi-file API write", branch)
	if err != nil {
		t.Fatalf("CreateBranchAndPush: %v", err)
	}
	t.Logf("pushed commit %s to branch %s", result.CommitSHA, result.Branch)

	// Verify each file.
	gh := newGitHubClient(token)
	for path, want := range files {
		data, err := gh.fetchRawBlob(ctx, owner, repo, result.CommitSHA, path)
		if err != nil {
			t.Errorf("fetch %s: %v", path, err)
			continue
		}
		if string(data) != want {
			t.Errorf("%s: got %q, want %q", path, string(data), want)
		}
	}

	// Cleanup.
	if err := gh.deleteRef(ctx, owner, repo, "heads/"+branch); err != nil {
		t.Logf("warning: failed to delete branch %s: %v", branch, err)
	} else {
		t.Logf("cleaned up branch %s", branch)
	}
}

// TestAPIWriteUnit tests the in-memory staging without hitting the API.
func TestAPIWriteUnit(t *testing.T) {
	be := &Backend{
		Ref:  RepoRef{Owner: "test", Repo: "test", Ref: "main", CommitSHA: "abc123"},
		tree: map[string]Entry{"existing.txt": {Path: "existing.txt", BlobSHA: "sha1"}},
	}
	be.cache.Store("sha1", []byte("old"))

	if err := be.WriteFileAPI("new.txt", []byte("hello")); err != nil {
		t.Fatalf("WriteFileAPI: %v", err)
	}
	if err := be.WriteFileAPI("another.txt", []byte("world")); err != nil {
		t.Fatalf("WriteFileAPI: %v", err)
	}

	pending := be.PendingFiles()
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(pending))
	}

	// Overwriting a pending file replaces it.
	if err := be.WriteFileAPI("new.txt", []byte("updated")); err != nil {
		t.Fatalf("WriteFileAPI overwrite: %v", err)
	}
	be.apiWrites.mu.Lock()
	if string(be.apiWrites.pending["new.txt"].Content) != "updated" {
		t.Error("overwrite did not replace content")
	}
	be.apiWrites.mu.Unlock()

	// Path validation.
	if err := be.WriteFileAPI("", []byte("x")); err == nil {
		t.Error("expected error for empty path")
	}
	if err := be.WriteFileAPI("../etc/passwd", []byte("x")); err == nil {
		t.Error("expected error for path with ..")
	}
}

// TestAPIWriteSubtree verifies that subtree-pinned writes produce correct repo-relative paths.
func TestAPIWriteSubtree(t *testing.T) {
	be := &Backend{
		Ref:  RepoRef{Owner: "test", Repo: "test", Ref: "main", CommitSHA: "abc", Subtree: "sub/dir"},
		tree: map[string]Entry{},
	}

	if err := be.WriteFileAPI("file.txt", []byte("data")); err != nil {
		t.Fatalf("WriteFileAPI: %v", err)
	}

	be.apiWrites.mu.Lock()
	pw := be.apiWrites.pending["file.txt"]
	be.apiWrites.mu.Unlock()

	if pw.Path != "sub/dir/file.txt" {
		t.Errorf("expected repo-relative path sub/dir/file.txt, got %s", pw.Path)
	}
}

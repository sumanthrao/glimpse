package gitbackend_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/surao/gitfs-accelerator/gitbackend"
)

func setupTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	commands := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	}
	for _, args := range commands {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	writeFile(t, dir, "hello.txt", "Hello, World!\n")
	writeFile(t, dir, "src/main.go", "package main\n\nfunc main() {}\n")
	writeFile(t, dir, "src/lib/util.go", "package lib\n\nfunc Add(a, b int) int { return a + b }\n")
	writeFile(t, dir, "docs/README.md", "# Test Project\n")

	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	cmd = exec.Command("git", "commit", "-m", "initial commit")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	return dir
}

func writeFile(t *testing.T, base, relPath, content string) {
	t.Helper()
	full := filepath.Join(base, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOpen(t *testing.T) {
	dir := setupTestRepo(t)

	b, err := gitbackend.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if b == nil {
		t.Fatal("backend is nil")
	}
}

func TestOpenInvalidPath(t *testing.T) {
	_, err := gitbackend.Open("/nonexistent/path")
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

func TestResolveRef(t *testing.T) {
	dir := setupTestRepo(t)
	b, err := gitbackend.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	hash, err := b.ResolveRef("HEAD")
	if err != nil {
		t.Fatalf("ResolveRef HEAD: %v", err)
	}
	if hash.IsZero() {
		t.Fatal("HEAD hash is zero")
	}

	hash2, err := b.ResolveRef("")
	if err != nil {
		t.Fatalf("ResolveRef empty: %v", err)
	}
	if hash != hash2 {
		t.Fatalf("empty ref and HEAD differ: %s vs %s", hash, hash2)
	}
}

func TestResolveRefBranch(t *testing.T) {
	dir := setupTestRepo(t)

	cmd := exec.Command("git", "branch", "feature")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}

	b, err := gitbackend.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	hash, err := b.ResolveRef("feature")
	if err != nil {
		t.Fatalf("ResolveRef feature: %v", err)
	}
	if hash.IsZero() {
		t.Fatal("feature hash is zero")
	}
}

func TestResolveRefTag(t *testing.T) {
	dir := setupTestRepo(t)

	cmd := exec.Command("git", "tag", "v1.0")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git tag: %v\n%s", err, out)
	}

	b, err := gitbackend.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	hash, err := b.ResolveRef("v1.0")
	if err != nil {
		t.Fatalf("ResolveRef v1.0: %v", err)
	}
	if hash.IsZero() {
		t.Fatal("tag hash is zero")
	}
}

func TestReadTree(t *testing.T) {
	dir := setupTestRepo(t)
	b, err := gitbackend.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	commitHash, _ := b.ResolveRef("HEAD")
	treeHash, err := b.RootTree(commitHash)
	if err != nil {
		t.Fatalf("RootTree: %v", err)
	}

	entries, err := b.ReadTree(treeHash)
	if err != nil {
		t.Fatalf("ReadTree: %v", err)
	}

	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}

	if !names["hello.txt"] {
		t.Error("missing hello.txt in root tree")
	}
	if !names["src"] {
		t.Error("missing src/ in root tree")
	}
	if !names["docs"] {
		t.Error("missing docs/ in root tree")
	}
}

func TestReadTreeIdentifiesDirs(t *testing.T) {
	dir := setupTestRepo(t)
	b, _ := gitbackend.Open(dir)
	commitHash, _ := b.ResolveRef("HEAD")
	treeHash, _ := b.RootTree(commitHash)
	entries, _ := b.ReadTree(treeHash)

	for _, e := range entries {
		switch e.Name {
		case "src", "docs":
			if !e.IsDir {
				t.Errorf("%s should be a directory", e.Name)
			}
			if e.TreeHash.IsZero() {
				t.Errorf("%s tree hash should not be zero", e.Name)
			}
		case "hello.txt":
			if e.IsDir {
				t.Error("hello.txt should not be a directory")
			}
			if e.BlobHash.IsZero() {
				t.Error("hello.txt blob hash should not be zero")
			}
		}
	}
}

func TestReadBlob(t *testing.T) {
	dir := setupTestRepo(t)
	b, _ := gitbackend.Open(dir)
	commitHash, _ := b.ResolveRef("HEAD")
	treeHash, _ := b.RootTree(commitHash)
	entries, _ := b.ReadTree(treeHash)

	for _, e := range entries {
		if e.Name == "hello.txt" {
			data, err := b.ReadBlob(e.BlobHash)
			if err != nil {
				t.Fatalf("ReadBlob: %v", err)
			}
			if string(data) != "Hello, World!\n" {
				t.Errorf("unexpected content: %q", string(data))
			}
			return
		}
	}
	t.Fatal("hello.txt not found")
}

func TestBlobSize(t *testing.T) {
	dir := setupTestRepo(t)
	b, _ := gitbackend.Open(dir)
	commitHash, _ := b.ResolveRef("HEAD")
	treeHash, _ := b.RootTree(commitHash)
	entries, _ := b.ReadTree(treeHash)

	for _, e := range entries {
		if e.Name == "hello.txt" {
			size, err := b.BlobSize(e.BlobHash)
			if err != nil {
				t.Fatalf("BlobSize: %v", err)
			}
			if size != 14 {
				t.Errorf("expected size 14, got %d", size)
			}
			return
		}
	}
}

func TestNestedTree(t *testing.T) {
	dir := setupTestRepo(t)
	b, _ := gitbackend.Open(dir)
	commitHash, _ := b.ResolveRef("HEAD")
	treeHash, _ := b.RootTree(commitHash)
	entries, _ := b.ReadTree(treeHash)

	var srcTreeHash = entries[0].TreeHash
	for _, e := range entries {
		if e.Name == "src" {
			srcTreeHash = e.TreeHash
			break
		}
	}

	srcEntries, err := b.ReadTree(srcTreeHash)
	if err != nil {
		t.Fatalf("ReadTree src: %v", err)
	}

	names := map[string]bool{}
	for _, e := range srcEntries {
		names[e.Name] = true
	}

	if !names["main.go"] {
		t.Error("missing main.go in src/")
	}
	if !names["lib"] {
		t.Error("missing lib/ in src/")
	}
}

func TestFileMode(t *testing.T) {
	dir := setupTestRepo(t)
	b, _ := gitbackend.Open(dir)

	if m := b.FileMode(0o100644); m != 0o644 {
		t.Errorf("regular file: expected 0644, got %o", m)
	}
	if m := b.FileMode(0o100755); m != 0o755 {
		t.Errorf("executable: expected 0755, got %o", m)
	}
	if m := b.FileMode(0o120000); m != 0o777 {
		t.Errorf("symlink: expected 0777, got %o", m)
	}
}

func TestTreeCaching(t *testing.T) {
	dir := setupTestRepo(t)
	b, _ := gitbackend.Open(dir)
	commitHash, _ := b.ResolveRef("HEAD")
	treeHash, _ := b.RootTree(commitHash)

	entries1, err := b.ReadTree(treeHash)
	if err != nil {
		t.Fatalf("first ReadTree: %v", err)
	}

	entries2, err := b.ReadTree(treeHash)
	if err != nil {
		t.Fatalf("second ReadTree: %v", err)
	}

	if len(entries1) != len(entries2) {
		t.Errorf("cached read returned different count: %d vs %d", len(entries1), len(entries2))
	}
}

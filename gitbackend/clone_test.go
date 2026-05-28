package gitbackend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateSparseCheckout_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := UpdateSparseCheckout(gitDir, "src/main.go"); err != nil {
		t.Fatalf("UpdateSparseCheckout: %v", err)
	}

	scPath := filepath.Join(gitDir, "info", "sparse-checkout")
	data, err := os.ReadFile(scPath)
	if err != nil {
		t.Fatalf("read sparse-checkout: %v", err)
	}
	content := strings.TrimSpace(string(data))
	if content != "/src/main.go" {
		t.Errorf("expected /src/main.go, got %q", content)
	}
}

func TestUpdateSparseCheckout_Deduplicates(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := UpdateSparseCheckout(gitDir, "src/main.go"); err != nil {
			t.Fatalf("UpdateSparseCheckout call %d: %v", i, err)
		}
	}
	scPath := filepath.Join(gitDir, "info", "sparse-checkout")
	data, _ := os.ReadFile(scPath)
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "/src/main.go" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 occurrence, got %d", count)
	}
}

func TestUpdateSparseCheckout_PreservesExisting(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	infoDir := filepath.Join(gitDir, "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scPath := filepath.Join(infoDir, "sparse-checkout")
	if err := os.WriteFile(scPath, []byte("/existing/file.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UpdateSparseCheckout(gitDir, "new/file.go"); err != nil {
		t.Fatalf("UpdateSparseCheckout: %v", err)
	}
	data, _ := os.ReadFile(scPath)
	content := string(data)
	if !strings.Contains(content, "/existing/file.txt") {
		t.Error("lost existing entry")
	}
	if !strings.Contains(content, "/new/file.go") {
		t.Error("missing new entry")
	}
}

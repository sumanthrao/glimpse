package fusefs

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var sparseMu sync.Mutex

// UpdateSparseCheckout appends a file path to .git/info/sparse-checkout
// so that git recognizes the materialized file as part of the working tree.
func UpdateSparseCheckout(gitDir, relPath string) error {
	sparseMu.Lock()
	defer sparseMu.Unlock()

	scPath := filepath.Join(gitDir, "info", "sparse-checkout")

	existing, err := readLines(scPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read sparse-checkout: %w", err)
	}

	normalized := "/" + relPath
	for _, line := range existing {
		if line == normalized {
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(scPath), 0o755); err != nil {
		return fmt.Errorf("create info dir: %w", err)
	}

	f, err := os.OpenFile(scPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open sparse-checkout: %w", err)
	}
	defer f.Close()

	if _, err := fmt.Fprintln(f, normalized); err != nil {
		return fmt.Errorf("write sparse-checkout: %w", err)
	}

	return nil
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

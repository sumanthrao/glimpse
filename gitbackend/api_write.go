package gitbackend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// PendingWrite records a staged file change for a future API commit.
type PendingWrite struct {
	Path    string // repo-relative (includes subtree prefix if pinned)
	Content []byte
}

// apiWriteState holds in-memory staged changes for the diskless write path.
type apiWriteState struct {
	mu      sync.Mutex
	pending map[string]PendingWrite // relative path -> PendingWrite
}

func (b *Backend) initAPIWriteState() {
	b.apiMu.Lock()
	defer b.apiMu.Unlock()
	if b.apiWrites == nil {
		b.apiWrites = &apiWriteState{
			pending: make(map[string]PendingWrite),
		}
	}
}

// WriteFileAPI stages a file write in memory without touching disk.
// The change is only pushed to GitHub when CommitAndPush is called.
// p is subtree-relative if the backend was opened with a subtree pin.
func (b *Backend) WriteFileAPI(p string, content []byte) error {
	p = NormalizePath(p)
	if p == "" {
		return fmt.Errorf("path is required")
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("path must not contain '..'")
	}
	b.initAPIWriteState()

	repoPath := p
	if b.Ref.Subtree != "" {
		repoPath = b.Ref.Subtree + "/" + p
	}

	b.apiWrites.mu.Lock()
	defer b.apiWrites.mu.Unlock()
	b.apiWrites.pending[p] = PendingWrite{Path: repoPath, Content: content}

	// Update RAM cache so subsequent reads see the staged content.
	if e, ok := b.tree[p]; ok && e.BlobSHA != "" {
		b.cache.Delete(e.BlobSHA)
	}
	b.cache.Store("staged:"+p, content)
	return nil
}

// PendingFiles returns the list of currently staged (uncommitted) file paths.
func (b *Backend) PendingFiles() []string {
	if b.apiWrites == nil {
		return nil
	}
	b.apiWrites.mu.Lock()
	defer b.apiWrites.mu.Unlock()
	paths := make([]string, 0, len(b.apiWrites.pending))
	for p := range b.apiWrites.pending {
		paths = append(paths, p)
	}
	return paths
}

// CommitAndPushResult holds the outcome of a successful diskless commit+push.
type CommitAndPushResult struct {
	CommitSHA string
	TreeSHA   string
	Branch    string
}

// CommitAndPush creates a commit with all pending writes via the GitHub Git
// Data API and fast-forwards the target branch. No disk, no git CLI.
//
// branch is the target branch name (e.g. "main"). If empty, uses the ref
// the backend was opened with.
//
// This is a non-force-push: if the branch tip has moved since we opened,
// the push will fail with a 422 and the caller must re-open or rebase.
func (b *Backend) CommitAndPush(ctx context.Context, message, branch string) (*CommitAndPushResult, error) {
	if b.apiWrites == nil || len(b.apiWrites.pending) == 0 {
		return nil, fmt.Errorf("no pending writes; call WriteFileAPI first")
	}
	if message == "" {
		return nil, fmt.Errorf("commit message is required")
	}
	if branch == "" {
		branch = b.Ref.Ref
	}
	if branch == "" {
		return nil, fmt.Errorf("branch is required (could not infer from ref)")
	}

	b.apiWrites.mu.Lock()
	writes := make([]PendingWrite, 0, len(b.apiWrites.pending))
	for _, w := range b.apiWrites.pending {
		writes = append(writes, w)
	}
	b.apiWrites.mu.Unlock()

	// Step 1: Create blobs for each file.
	type blobResult struct {
		path string
		sha  string
	}
	blobs := make([]blobResult, len(writes))
	for i, w := range writes {
		sha, err := b.gh.createBlob(ctx, b.Ref.Owner, b.Ref.Repo, w.Content)
		if err != nil {
			return nil, fmt.Errorf("create blob for %s: %w", w.Path, err)
		}
		blobs[i] = blobResult{path: w.Path, sha: sha}
	}

	// Step 2: Create a new tree with the blobs patched in.
	treeEntries := make([]newTreeEntry, len(blobs))
	for i, bl := range blobs {
		treeEntries[i] = newTreeEntry{
			Path: bl.path,
			Mode: "100644",
			Type: "blob",
			SHA:  bl.sha,
		}
	}
	treeSHA, err := b.gh.createTree(ctx, b.Ref.Owner, b.Ref.Repo, b.Ref.CommitSHA, treeEntries)
	if err != nil {
		return nil, fmt.Errorf("create tree: %w", err)
	}

	// Step 3: Create a commit.
	commitSHA, err := b.gh.createCommit(ctx, b.Ref.Owner, b.Ref.Repo, message, treeSHA, b.Ref.CommitSHA)
	if err != nil {
		return nil, fmt.Errorf("create commit: %w", err)
	}

	// Step 4: Update the branch ref to point at the new commit.
	// Try update first; if the branch doesn't exist yet, create it.
	if err := b.gh.updateRef(ctx, b.Ref.Owner, b.Ref.Repo, "heads/"+branch, commitSHA, false); err != nil {
		if strings.Contains(err.Error(), "422") || strings.Contains(err.Error(), "Reference does not exist") {
			if err2 := b.gh.createRef(ctx, b.Ref.Owner, b.Ref.Repo, "refs/heads/"+branch, commitSHA); err2 != nil {
				return nil, fmt.Errorf("create ref heads/%s: %w (update also failed: %v)", branch, err2, err)
			}
		} else {
			return nil, fmt.Errorf("update ref heads/%s: %w", branch, err)
		}
	}

	// Clear pending writes and update backend state.
	b.apiWrites.mu.Lock()
	b.apiWrites.pending = make(map[string]PendingWrite)
	b.apiWrites.mu.Unlock()
	b.Ref.CommitSHA = commitSHA

	return &CommitAndPushResult{
		CommitSHA: commitSHA,
		TreeSHA:   treeSHA,
		Branch:    branch,
	}, nil
}

// CreateBranchAndPush creates a new branch from the current commit, then
// commits and pushes all pending writes to that branch. Useful for PR workflows.
func (b *Backend) CreateBranchAndPush(ctx context.Context, message, newBranch string) (*CommitAndPushResult, error) {
	if newBranch == "" {
		return nil, fmt.Errorf("new branch name is required")
	}
	// Create the branch ref pointing at the current commit.
	if err := b.gh.createRef(ctx, b.Ref.Owner, b.Ref.Repo, "refs/heads/"+newBranch, b.Ref.CommitSHA); err != nil {
		return nil, fmt.Errorf("create branch %s: %w", newBranch, err)
	}
	return b.CommitAndPush(ctx, message, newBranch)
}

// ---------------------------------------------------------------------------
// GitHub Git Data API methods on githubClient
// ---------------------------------------------------------------------------

// createBlob creates a blob in the repo and returns its SHA.
func (c *githubClient) createBlob(ctx context.Context, owner, repo string, content []byte) (string, error) {
	body := map[string]string{
		"content":  base64.StdEncoding.EncodeToString(content),
		"encoding": "base64",
	}
	var resp struct {
		SHA string `json:"sha"`
	}
	err := c.postJSON(ctx, fmt.Sprintf("/repos/%s/%s/git/blobs", urlEscape(owner), urlEscape(repo)), body, &resp)
	if err != nil {
		return "", err
	}
	return resp.SHA, nil
}

type newTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// createTree creates a new tree with the given entries, based on the parent
// commit's tree SHA. Returns the new tree SHA.
func (c *githubClient) createTree(ctx context.Context, owner, repo, baseCommitSHA string, entries []newTreeEntry) (string, error) {
	body := map[string]any{
		"base_tree": baseCommitSHA,
		"tree":      entries,
	}
	var resp struct {
		SHA string `json:"sha"`
	}
	err := c.postJSON(ctx, fmt.Sprintf("/repos/%s/%s/git/trees", urlEscape(owner), urlEscape(repo)), body, &resp)
	if err != nil {
		return "", err
	}
	return resp.SHA, nil
}

// createCommit creates a commit object. Returns the commit SHA.
func (c *githubClient) createCommit(ctx context.Context, owner, repo, message, treeSHA, parentSHA string) (string, error) {
	body := map[string]any{
		"message": message,
		"tree":    treeSHA,
		"parents": []string{parentSHA},
	}
	var resp struct {
		SHA string `json:"sha"`
	}
	err := c.postJSON(ctx, fmt.Sprintf("/repos/%s/%s/git/commits", urlEscape(owner), urlEscape(repo)), body, &resp)
	if err != nil {
		return "", err
	}
	return resp.SHA, nil
}

// updateRef updates an existing ref to point at a new SHA.
func (c *githubClient) updateRef(ctx context.Context, owner, repo, ref, sha string, force bool) error {
	body := map[string]any{
		"sha":   sha,
		"force": force,
	}
	return c.postJSON(ctx, fmt.Sprintf("/repos/%s/%s/git/refs/%s", urlEscape(owner), urlEscape(repo), ref), body, nil)
}

// createRef creates a new ref (e.g. "refs/heads/my-branch") at the given SHA.
func (c *githubClient) createRef(ctx context.Context, owner, repo, ref, sha string) error {
	body := map[string]any{
		"ref": ref,
		"sha": sha,
	}
	return c.postJSON(ctx, fmt.Sprintf("/repos/%s/%s/git/refs", urlEscape(owner), urlEscape(repo)), body, nil)
}

// deleteRef deletes a ref (for test cleanup).
func (c *githubClient) deleteRef(ctx context.Context, owner, repo, ref string) error {
	path := fmt.Sprintf("/repos/%s/%s/git/refs/%s", urlEscape(owner), urlEscape(repo), ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "https://api.github.com"+path, nil)
	if err != nil {
		return err
	}
	c.addAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("delete ref %s: %s: %s", ref, resp.Status, string(body))
	}
	return nil
}

// postJSON POSTs a JSON body to a GitHub REST endpoint and decodes the response.
func (c *githubClient) postJSON(ctx context.Context, path string, body any, result any) error {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return err
	}

	method := http.MethodPost
	// PATCH for ref updates.
	if strings.Contains(path, "/git/refs/") && !strings.HasSuffix(path, "/git/refs") {
		method = http.MethodPatch
	}

	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	c.addAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	c.recordRate(resp.Header, false)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return c.rateOrAuthError(resp)
	}
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(respBody)))
	}
	if result == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(result)
}

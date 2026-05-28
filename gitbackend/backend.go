// Package gitbackend is glimpse's GitHub-only repository backend.
//
// One backend instance corresponds to one (owner, repo, ref) tuple. Open
// fetches the recursive git tree from GitHub and caches every entry's path,
// mode, blob SHA, and size. From there:
//
//   - AccessFile is the single bridge that turns a path into bytes. It
//     consults the in-memory blob cache, then the local worktree (if writes
//     have triggered a lazy clone), and finally the raw.githubusercontent.com
//     CDN. Concurrent calls for the same blob SHA are deduped by singleflight.
//
//   - Every successful AccessFile result is added to the IncrementalIndex —
//     a trigram inverted index over the working set the agent has actually
//     touched. Grep merges this with GitHub Code Search hits.
//
//   - Writes lazily provision a partial bare clone (--filter=blob:none) plus a
//     sparse worktree under the cache dir. Reads still prefer the CDN even
//     after that; the worktree only serves files the agent has materialized.
package gitbackend

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
)

// Entry is a single tree entry returned by GitHub's recursive tree API.
type Entry struct {
	Path    string
	IsDir   bool
	Mode    uint32 // POSIX file mode bits, derived from git mode string
	BlobSHA string // empty for directories
	Size    int64
}

// Backend is the GitHub-backed VFS. Safe for concurrent use.
type Backend struct {
	Ref      RepoRef
	cacheDir string

	gh *githubClient

	tree     map[string]Entry      // path -> Entry
	children map[string][]Entry    // parent path -> direct children, sorted by Path
	treeMu   sync.RWMutex

	cache    sync.Map // blob SHA -> []byte; deduped across paths sharing content
	inflight singleflight.Group

	sem *semaphore.Weighted // bounds concurrent CDN fetches

	idx *IncrementalIndex

	// Lazy clone state for writes.
	cloneOnce sync.Once
	cloneErr  error
	bareDir   string
	wtDir     string

	// Stats counters (atomic).
	blobsFetched atomic.Int64
	bytesFetched atomic.Int64
	cdnHits      atomic.Int64
	diskHits     atomic.Int64
	ramHits      atomic.Int64

	// Repo metadata cached at Open.
	defaultBranch string
	languages     map[string]int64
	private       bool

	// truncated is set when the GitHub Trees API returned a partial result
	// (the repo or pinned subtree exceeds ~7 MB / 100k entries). The backend
	// still operates on the entries it did receive; callers that care can
	// surface this to the user via Stats.
	truncated bool
}

// Open fetches the tree for ref and constructs a backend. Token is optional;
// when set, REST and Code Search rate limits go up considerably and private
// repos become accessible.
func Open(ctx context.Context, ref RepoRef, token, cacheDir string) (*Backend, error) {
	if cacheDir == "" {
		home, _ := os.UserHomeDir()
		cacheDir = filepath.Join(home, ".cache", "glimpse")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	gh := newGitHubClient(token)

	// Resolve user-supplied ref (which may be empty) to a stable commit SHA.
	// resolvedInfo is non-nil when ref was empty — we got the repo metadata
	// along the way and can reuse it to avoid a duplicate fetch below.
	resolvedRef, sha, resolvedInfo, err := gh.resolveCommit(ctx, ref.Owner, ref.Repo, ref.Ref)
	if err != nil {
		return nil, fmt.Errorf("resolve ref: %w", err)
	}
	ref.Ref = resolvedRef
	ref.CommitSHA = sha
	ref.Subtree = NormalizePath(ref.Subtree)

	// Choose the tree-ish SHA to fetch from. For subtree-pinned URLs we walk
	// down from the commit's root tree to the requested directory and fetch
	// just that subtree, which keeps glimpse usable on monorepos that exceed
	// the Trees API ~7 MB / 100k-entry cap.
	treeSHA := sha
	if ref.Subtree != "" {
		subSHA, err := gh.resolveSubtreeSHA(ctx, ref.Owner, ref.Repo, sha, ref.Subtree)
		if err != nil {
			return nil, fmt.Errorf("resolve subtree %q: %w", ref.Subtree, err)
		}
		treeSHA = subSHA
	}

	// Tree fetch + repo metadata + languages are all independent of each
	// other once the commit SHA is resolved. Fire them in parallel so the
	// open path is bounded by the slowest single call (the tree) rather
	// than the sum of three round trips.
	var (
		tr   *treeResponse
		info repoInfo
		langs map[string]int64
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		t, err := gh.fetchTree(gctx, ref.Owner, ref.Repo, treeSHA)
		if err != nil {
			return fmt.Errorf("fetch tree: %w", err)
		}
		tr = t
		return nil
	})
	g.Go(func() error {
		// Reuse metadata fetched during ref resolution when available.
		if resolvedInfo != nil {
			info = *resolvedInfo
			return nil
		}
		// Best-effort; failures are tolerated.
		_ = gh.doJSON(gctx, fmt.Sprintf("/repos/%s/%s", urlEscape(ref.Owner), urlEscape(ref.Repo)), &info)
		return nil
	})
	g.Go(func() error {
		langs, _ = gh.fetchLanguages(gctx, ref.Owner, ref.Repo)
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	if tr.Truncated {
		// Don't fail. The Trees API truncated the response (~7 MB / 100k
		// entries). Whatever entries we did receive are still usable; the
		// truncation is surfaced via Stats.Truncated and a stderr warning.
		hint := "pin to a subtree by appending /tree/<branch>/<path> to the URL"
		if ref.Subtree != "" {
			hint = fmt.Sprintf("the pinned subtree %q is itself too large; pin deeper, e.g. /tree/<branch>/%s/<subdir>", ref.Subtree, ref.Subtree)
		}
		fmt.Fprintf(os.Stderr,
			"glimpse: warning: tree was truncated by GitHub for %s; results may be partial. Hint: %s\n",
			ref.String(), hint)
	}

	tree := make(map[string]Entry, len(tr.Tree))
	children := map[string][]Entry{}
	for _, e := range tr.Tree {
		isDir := e.Type == "tree"
		mode := parseGitMode(e.Mode, isDir)
		entry := Entry{
			Path:    e.Path,
			IsDir:   isDir,
			Mode:    mode,
			BlobSHA: e.SHA,
			Size:    e.Size,
		}
		if e.Type != "blob" && e.Type != "tree" {
			// Submodules ("commit") and any other types are surfaced as
			// directories with no children — we don't follow into them.
			entry.IsDir = true
		}
		tree[e.Path] = entry
		parent := path.Dir(e.Path)
		if parent == "." {
			parent = ""
		}
		children[parent] = append(children[parent], entry)
	}

	return &Backend{
		Ref:           ref,
		cacheDir:      cacheDir,
		gh:            gh,
		tree:          tree,
		children:      children,
		sem:           semaphore.NewWeighted(8),
		idx:           newIncrementalIndex(),
		defaultBranch: info.DefaultBranch,
		languages:     langs,
		private:       info.Private,
		truncated:     tr.Truncated,
	}, nil
}

// Tree returns a snapshot reference to the tree map. Do not mutate. Cheap to
// call repeatedly — readers take a read lock; the map itself is immutable
// after Open.
func (b *Backend) Tree() map[string]Entry {
	return b.tree
}

// Children returns the direct children of the given directory path. Empty
// string is the repo root. Returned slice must not be mutated.
func (b *Backend) Children(dir string) []Entry {
	dir = NormalizePath(dir)
	c, ok := b.children[dir]
	if !ok {
		return nil
	}
	return c
}

// Lookup finds an entry by path. Empty string is the implicit root directory.
func (b *Backend) Lookup(p string) (Entry, bool) {
	p = NormalizePath(p)
	if p == "" {
		return Entry{Path: "", IsDir: true, Mode: 0o755}, true
	}
	e, ok := b.tree[p]
	return e, ok
}

// Index exposes the incremental working-set index.
func (b *Backend) Index() *IncrementalIndex { return b.idx }

// Languages returns a copy of the language byte map, or nil.
func (b *Backend) Languages() map[string]int64 {
	if b.languages == nil {
		return nil
	}
	out := make(map[string]int64, len(b.languages))
	for k, v := range b.languages {
		out[k] = v
	}
	return out
}

// Stats summarizes runtime state for repo_status.
type Stats struct {
	BlobsFetched int64
	BytesFetched int64
	RAMHits      int64
	DiskHits     int64
	CDNHits      int64
	Files        int
	Dirs         int
	Index        IndexStats
	Rate         RateSnapshot
	Writable     bool
	Private      bool
	WorktreeDir  string
	Truncated    bool   // tree fetch hit the GitHub Trees API cap
	Subtree      string // pinned subtree path, if any
}

func (b *Backend) Stats() Stats {
	files, dirs := 0, 0
	for _, e := range b.tree {
		if e.IsDir {
			dirs++
		} else {
			files++
		}
	}
	return Stats{
		BlobsFetched: b.blobsFetched.Load(),
		BytesFetched: b.bytesFetched.Load(),
		RAMHits:      b.ramHits.Load(),
		DiskHits:     b.diskHits.Load(),
		CDNHits:      b.cdnHits.Load(),
		Files:        files,
		Dirs:         dirs,
		Index:        b.idx.Stats(),
		Rate:         b.gh.Rate(),
		Writable:     b.wtDir != "",
		Private:      b.private,
		WorktreeDir:  b.wtDir,
		Truncated:    b.truncated,
		Subtree:      b.Ref.Subtree,
	}
}

// AccessFile is THE bridge interface. Every byte the backend hands out for a
// path comes through here. Three-tier resolution: RAM blob cache (by SHA),
// then local disk if a worktree has been provisioned, then the raw CDN.
//
// On success, the returned bytes are cached by blob SHA and added to the
// IncrementalIndex so future searches can match against them without another
// round trip.
func (b *Backend) AccessFile(ctx context.Context, p string) ([]byte, error) {
	p = NormalizePath(p)
	e, ok := b.tree[p]
	if !ok {
		return nil, fmt.Errorf("path not found: %q (try find_files to locate it)", p)
	}
	if e.IsDir {
		return nil, fmt.Errorf("not a file: %q is a directory", p)
	}

	// Tier 1: RAM cache (deduped by blob SHA so two paths with same content
	// share storage).
	if v, ok := b.cache.Load(e.BlobSHA); ok {
		b.ramHits.Add(1)
		return v.([]byte), nil
	}

	// Tier 2: local disk if worktree exists and the file has been materialized
	// (writes only). We do NOT consult the bare clone's pack — it has no blobs
	// under --filter=blob:none. Worktree paths are full repo-relative even
	// when the backend is subtree-pinned.
	if b.wtDir != "" {
		diskPath := p
		if b.Ref.Subtree != "" {
			diskPath = b.Ref.Subtree + "/" + p
		}
		if data, err := os.ReadFile(filepath.Join(b.wtDir, diskPath)); err == nil {
			b.diskHits.Add(1)
			b.populate(p, e.BlobSHA, data)
			return data, nil
		}
	}

	// Tier 3: CDN fetch, deduped by blob SHA. Paths in the tree map are
	// relative to b.Ref.Subtree (when set), but raw.githubusercontent.com
	// expects the full repo-relative path.
	rawPath := p
	if b.Ref.Subtree != "" {
		rawPath = b.Ref.Subtree + "/" + p
	}
	v, err, _ := b.inflight.Do(e.BlobSHA, func() (any, error) {
		// Re-check cache after acquiring inflight slot — another caller may
		// have populated it while we were waiting.
		if v, ok := b.cache.Load(e.BlobSHA); ok {
			return v, nil
		}
		if err := b.sem.Acquire(ctx, 1); err != nil {
			return nil, err
		}
		defer b.sem.Release(1)
		data, err := b.gh.fetchRawBlob(ctx, b.Ref.Owner, b.Ref.Repo, b.Ref.CommitSHA, rawPath)
		if err != nil {
			return nil, err
		}
		b.cdnHits.Add(1)
		b.blobsFetched.Add(1)
		b.bytesFetched.Add(int64(len(data)))
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	data := v.([]byte)
	b.populate(p, e.BlobSHA, data)
	return data, nil
}

// AccessFiles fetches multiple paths in parallel, bounded by the backend
// semaphore. Returns a partial result map plus any first-encountered error
// (the rest of the fetches still run).
func (b *Backend) AccessFiles(ctx context.Context, paths []string) (map[string][]byte, error) {
	out := sync.Map{}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(16) // upper bound on goroutines; sem still bounds network concurrency
	for _, p := range paths {
		p := p
		g.Go(func() error {
			data, err := b.AccessFile(gctx, p)
			if err != nil {
				return nil // collect best-effort; caller can re-query individuals on miss
			}
			out.Store(p, data)
			return nil
		})
	}
	err := g.Wait()
	result := map[string][]byte{}
	out.Range(func(k, v any) bool {
		result[k.(string)] = v.([]byte)
		return true
	})
	return result, err
}

func (b *Backend) populate(path, sha string, data []byte) {
	b.cache.Store(sha, data)
	b.idx.Add(path, sha, data)
}

// NormalizePath strips leading/trailing slashes and resolves "." to "".
// Public so MCP / FUSE callers can normalize without re-implementing it.
func NormalizePath(p string) string {
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	if p == "." {
		return ""
	}
	return p
}

// parseGitMode converts git's octal mode string to a POSIX file mode. The
// mode bits returned are usable directly for FUSE Getattr.
func parseGitMode(mode string, isDir bool) uint32 {
	if isDir {
		return 0o755
	}
	m, _ := strconv.ParseUint(mode, 8, 32)
	switch m {
	case 0o100755: // executable
		return 0o755
	case 0o120000: // symlink
		return 0o777
	case 0o160000: // submodule (gitlink)
		return 0o755
	default:
		return 0o644
	}
}

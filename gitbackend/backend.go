package gitbackend

import (
	"fmt"
	"io"
	"sync"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type Entry struct {
	Name    string
	IsDir   bool
	Mode    uint32
	BlobHash plumbing.Hash
	TreeHash plumbing.Hash
	Size     int64
}

type Backend struct {
	repo *git.Repository
	mu   sync.RWMutex

	// Cache tree objects to avoid repeated lookups
	treeCache map[plumbing.Hash]*object.Tree
	treeMu    sync.RWMutex
}

func Open(repoPath string) (*Backend, error) {
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		// Try as bare repo
		repo, err = git.PlainOpenWithOptions(repoPath, &git.PlainOpenOptions{
			DetectDotGit: true,
		})
		if err != nil {
			return nil, fmt.Errorf("open git repo at %s: %w", repoPath, err)
		}
	}

	return &Backend{
		repo:      repo,
		treeCache: make(map[plumbing.Hash]*object.Tree),
	}, nil
}

func (b *Backend) ResolveRef(refName string) (plumbing.Hash, error) {
	if refName == "" {
		refName = "HEAD"
	}

	ref, err := b.repo.Reference(plumbing.ReferenceName(refName), true)
	if err != nil {
		// Try as short branch name
		ref, err = b.repo.Reference(plumbing.ReferenceName("refs/heads/"+refName), true)
		if err != nil {
			// Try as tag
			ref, err = b.repo.Reference(plumbing.ReferenceName("refs/tags/"+refName), true)
			if err != nil {
				// Try parsing as a raw hash
				hash := plumbing.NewHash(refName)
				if hash.IsZero() {
					return plumbing.ZeroHash, fmt.Errorf("cannot resolve ref %q", refName)
				}
				return hash, nil
			}
		}
	}

	return ref.Hash(), nil
}

func (b *Backend) RootTree(commitHash plumbing.Hash) (plumbing.Hash, error) {
	commit, err := b.repo.CommitObject(commitHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("get commit %s: %w", commitHash, err)
	}
	return commit.TreeHash, nil
}

func (b *Backend) ReadTree(hash plumbing.Hash) ([]Entry, error) {
	tree, err := b.getTree(hash)
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		entry := Entry{
			Name: e.Name,
			Mode: uint32(e.Mode),
		}

		switch {
		case e.Mode == filemode.Dir:
			entry.IsDir = true
			entry.TreeHash = e.Hash
		default:
			entry.IsDir = false
			entry.BlobHash = e.Hash
			blob, err := b.repo.BlobObject(e.Hash)
			if err == nil {
				entry.Size = blob.Size
			}
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

func (b *Backend) ReadBlob(hash plumbing.Hash) ([]byte, error) {
	blob, err := b.repo.BlobObject(hash)
	if err != nil {
		return nil, fmt.Errorf("get blob %s: %w", hash, err)
	}

	reader, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", hash, err)
	}
	defer reader.Close()

	return io.ReadAll(reader)
}

func (b *Backend) BlobSize(hash plumbing.Hash) (int64, error) {
	blob, err := b.repo.BlobObject(hash)
	if err != nil {
		return 0, err
	}
	return blob.Size, nil
}

func (b *Backend) getTree(hash plumbing.Hash) (*object.Tree, error) {
	b.treeMu.RLock()
	if t, ok := b.treeCache[hash]; ok {
		b.treeMu.RUnlock()
		return t, nil
	}
	b.treeMu.RUnlock()

	b.treeMu.Lock()
	defer b.treeMu.Unlock()

	// Double-check after acquiring write lock
	if t, ok := b.treeCache[hash]; ok {
		return t, nil
	}

	tree, err := b.repo.TreeObject(hash)
	if err != nil {
		return nil, fmt.Errorf("get tree %s: %w", hash, err)
	}

	b.treeCache[hash] = tree
	return tree, nil
}

func (b *Backend) FileMode(mode uint32) uint32 {
	switch filemode.FileMode(mode) {
	case filemode.Executable:
		return 0o755
	case filemode.Symlink:
		return 0o777
	default:
		return 0o644
	}
}

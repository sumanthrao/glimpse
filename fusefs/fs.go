// Package fusefs mounts a glimpse Backend as a FUSE filesystem.
//
// The mount serves directory listings from the in-memory tree map and file
// reads through Backend.AccessFile (lazy CDN fetch + cache + index update).
// Writes lazily provision a worktree under the cache directory; once a path
// has been written, FUSE flips that path to disk-backed reads.
package fusefs

import (
	"context"
	"path"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/surao/gitfs-accelerator/gitbackend"
)

// GitFS is the FUSE root.
type GitFS struct {
	fs.Inode

	backend *gitbackend.Backend
}

// NewGitFS constructs a FUSE root over a backend.
func NewGitFS(backend *gitbackend.Backend) *GitFS {
	return &GitFS{backend: backend}
}

var _ = (fs.NodeOnAdder)((*GitFS)(nil))

func (g *GitFS) OnAdd(ctx context.Context) {
	g.populate(&g.Inode, "")
}

// populate builds child inodes for one directory level. Called eagerly for
// the root and lazily for subdirs (via gitDir.ensurePopulated).
func (g *GitFS) populate(parent *fs.Inode, dir string) {
	for _, e := range g.backend.Children(dir) {
		name := path.Base(e.Path)
		if e.IsDir {
			d := &gitDir{gitFS: g, relPath: e.Path}
			parent.AddChild(name, parent.NewPersistentInode(context.Background(), d, fs.StableAttr{Mode: syscall.S_IFDIR}), true)
		} else {
			f := &gitFile{
				gitFS:   g,
				relPath: e.Path,
				size:    uint64(e.Size),
				mode:    e.Mode,
			}
			parent.AddChild(name, parent.NewPersistentInode(context.Background(), f, fs.StableAttr{}), true)
		}
	}
}

// gitDir is a directory backed by the backend's tree map. Children are
// populated lazily on first Lookup/Readdir.
type gitDir struct {
	fs.Inode

	gitFS     *GitFS
	relPath   string
	once      sync.Once
}

func (d *gitDir) ensurePopulated() {
	d.once.Do(func() {
		d.gitFS.populate(&d.Inode, d.relPath)
	})
}

var _ = (fs.NodeLookuper)((*gitDir)(nil))

func (d *gitDir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	d.ensurePopulated()
	child := d.GetChild(name)
	if child == nil {
		return nil, syscall.ENOENT
	}
	return child, 0
}

var _ = (fs.NodeReaddirer)((*gitDir)(nil))

func (d *gitDir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	d.ensurePopulated()
	children := d.Children()
	entries := make([]fuse.DirEntry, 0, len(children))
	for name, child := range children {
		entries = append(entries, fuse.DirEntry{Name: name, Mode: child.Mode()})
	}
	return fs.NewListDirStream(entries), 0
}

var _ = (fs.NodeGetattrer)((*gitDir)(nil))

func (d *gitDir) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o755
	now := time.Now()
	out.SetTimes(&now, &now, &now)
	return 0
}

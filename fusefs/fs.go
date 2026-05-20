package fusefs

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/surao/gitfs-accelerator/gitbackend"
)

type GitFS struct {
	fs.Inode

	backend  *gitbackend.Backend
	treeHash plumbing.Hash
	workdir  string // the real git worktree on disk
	gitDir   string // path to .git directory

	mu                   sync.Mutex
	blobsRead            int64
	bytesRead            int64
	dirReads             int64
	diskMaterializations int64
}

type Stats struct {
	BlobsRead            int64
	BytesRead            int64
	DirReads             int64
	DiskMaterializations int64
}

func (g *GitFS) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Stats{
		BlobsRead:            g.blobsRead,
		BytesRead:            g.bytesRead,
		DirReads:             g.dirReads,
		DiskMaterializations: g.diskMaterializations,
	}
}

func NewGitFS(backend *gitbackend.Backend, treeHash plumbing.Hash, workdir, gitDir string) *GitFS {
	return &GitFS{
		backend:  backend,
		treeHash: treeHash,
		workdir:  workdir,
		gitDir:   gitDir,
	}
}

var _ = (fs.NodeOnAdder)((*GitFS)(nil))

func (g *GitFS) OnAdd(ctx context.Context) {
	g.populateDir(&g.Inode, g.treeHash, "")
}

func (g *GitFS) populateDir(parent *fs.Inode, treeHash plumbing.Hash, relPath string) {
	entries, err := g.backend.ReadTree(treeHash)
	if err != nil {
		log.Printf("error reading tree %s: %v", treeHash, err)
		return
	}

	g.mu.Lock()
	g.dirReads++
	g.mu.Unlock()

	for _, entry := range entries {
		childPath := filepath.Join(relPath, entry.Name)

		if entry.IsDir {
			dirNode := &gitDir{
				gitFS:     g,
				treeHash:  entry.TreeHash,
				relPath:   childPath,
				populated: false,
			}
			child := parent.NewPersistentInode(context.Background(), dirNode, fs.StableAttr{Mode: syscall.S_IFDIR})
			parent.AddChild(entry.Name, child, true)
		} else {
			mode := g.backend.FileMode(entry.Mode)
			diskPath := filepath.Join(g.workdir, childPath)

			fileNode := &gitFile{
				gitFS:    g,
				blobHash: entry.BlobHash,
				size:     uint64(entry.Size),
				mode:     mode,
				relPath:  childPath,
				diskPath: diskPath,
			}

			child := parent.NewPersistentInode(context.Background(), fileNode, fs.StableAttr{})
			parent.AddChild(entry.Name, child, true)
		}
	}
}

// gitDir represents a directory backed by a git tree object.
// Lazily populates children on first Readdir/Lookup.
type gitDir struct {
	fs.Inode

	gitFS     *GitFS
	treeHash  plumbing.Hash
	relPath   string
	populated bool
	mu        sync.Mutex
}

var _ = (fs.NodeOnAdder)((*gitDir)(nil))

func (d *gitDir) OnAdd(ctx context.Context) {}

func (d *gitDir) ensurePopulated() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.populated {
		return
	}
	d.gitFS.populateDir(&d.Inode, d.treeHash, d.relPath)
	d.populated = true
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
	entries := make([]fuse.DirEntry, 0)
	for name, child := range children {
		entries = append(entries, fuse.DirEntry{
			Name: name,
			Mode: child.Mode(),
		})
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

var _ = (fs.NodeMkdirer)((*gitDir)(nil))

func (d *gitDir) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	d.ensurePopulated()

	dirPath := filepath.Join(d.gitFS.workdir, d.relPath, name)
	if err := os.MkdirAll(dirPath, os.FileMode(mode)); err != nil {
		return nil, syscall.EIO
	}

	newDir := &realDir{
		gitFS:   d.gitFS,
		dirPath: dirPath,
		relPath: filepath.Join(d.relPath, name),
	}
	child := d.NewPersistentInode(ctx, newDir, fs.StableAttr{Mode: syscall.S_IFDIR})
	d.AddChild(name, child, true)

	out.Mode = mode
	return child, 0
}

var _ = (fs.NodeCreater)((*gitDir)(nil))

func (d *gitDir) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	d.ensurePopulated()

	filePath := filepath.Join(d.gitFS.workdir, d.relPath, name)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return nil, nil, 0, syscall.EIO
	}

	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(mode))
	if err != nil {
		return nil, nil, 0, syscall.EIO
	}
	f.Close()

	newFile := &realFile{
		gitFS:    d.gitFS,
		filePath: filePath,
		relPath:  filepath.Join(d.relPath, name),
	}
	child := d.NewPersistentInode(ctx, newFile, fs.StableAttr{})
	d.AddChild(name, child, true)

	out.Mode = mode
	return child, nil, 0, 0
}

// realDir represents a directory created by the user on the real worktree.
type realDir struct {
	fs.Inode

	gitFS   *GitFS
	dirPath string
	relPath string
}

var _ = (fs.NodeGetattrer)((*realDir)(nil))
var _ = (fs.NodeReaddirer)((*realDir)(nil))
var _ = (fs.NodeLookuper)((*realDir)(nil))
var _ = (fs.NodeMkdirer)((*realDir)(nil))
var _ = (fs.NodeCreater)((*realDir)(nil))

func (d *realDir) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o755
	now := time.Now()
	out.SetTimes(&now, &now, &now)
	return 0
}

func (d *realDir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	dirEntries, err := os.ReadDir(d.dirPath)
	if err != nil {
		return fs.NewListDirStream(nil), 0
	}

	entries := make([]fuse.DirEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		mode := uint32(0o644)
		if de.IsDir() {
			mode = syscall.S_IFDIR | 0o755
		}
		entries = append(entries, fuse.DirEntry{
			Name: de.Name(),
			Mode: mode,
		})
	}
	return fs.NewListDirStream(entries), 0
}

func (d *realDir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childPath := filepath.Join(d.dirPath, name)
	info, err := os.Stat(childPath)
	if err != nil {
		return nil, syscall.ENOENT
	}

	childRel := filepath.Join(d.relPath, name)

	if info.IsDir() {
		child := d.NewPersistentInode(ctx, &realDir{
			gitFS:   d.gitFS,
			dirPath: childPath,
			relPath: childRel,
		}, fs.StableAttr{Mode: syscall.S_IFDIR})
		return child, 0
	}

	child := d.NewPersistentInode(ctx, &realFile{
		gitFS:    d.gitFS,
		filePath: childPath,
		relPath:  childRel,
	}, fs.StableAttr{})
	return child, 0
}

func (d *realDir) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childPath := filepath.Join(d.dirPath, name)
	if err := os.MkdirAll(childPath, os.FileMode(mode)); err != nil {
		return nil, syscall.EIO
	}

	child := d.NewPersistentInode(ctx, &realDir{
		gitFS:   d.gitFS,
		dirPath: childPath,
		relPath: filepath.Join(d.relPath, name),
	}, fs.StableAttr{Mode: syscall.S_IFDIR})
	d.AddChild(name, child, true)

	out.Mode = mode
	return child, 0
}

func (d *realDir) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	childPath := filepath.Join(d.dirPath, name)

	f, err := os.OpenFile(childPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(mode))
	if err != nil {
		return nil, nil, 0, syscall.EIO
	}
	f.Close()

	child := d.NewPersistentInode(ctx, &realFile{
		gitFS:    d.gitFS,
		filePath: childPath,
		relPath:  filepath.Join(d.relPath, name),
	}, fs.StableAttr{})
	d.AddChild(name, child, true)

	out.Mode = mode
	return child, nil, 0, 0
}

// realFile is a file that already exists on disk (user-created, not from git).
type realFile struct {
	fs.Inode

	gitFS    *GitFS
	filePath string
	relPath  string
}

var _ = (fs.NodeGetattrer)((*realFile)(nil))
var _ = (fs.NodeOpener)((*realFile)(nil))
var _ = (fs.NodeReader)((*realFile)(nil))
var _ = (fs.NodeWriter)((*realFile)(nil))

func (f *realFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	info, err := os.Stat(f.filePath)
	if err != nil {
		out.Mode = 0o644
		now := time.Now()
		out.SetTimes(&now, &now, &now)
		return 0
	}
	out.Size = uint64(info.Size())
	out.Mode = uint32(info.Mode())
	modTime := info.ModTime()
	out.SetTimes(&modTime, &modTime, &modTime)
	return 0
}

func (f *realFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (f *realFile) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	data, err := os.ReadFile(f.filePath)
	if err != nil {
		return nil, syscall.EIO
	}
	end := int(off) + len(dest)
	if end > len(data) {
		end = len(data)
	}
	if int(off) >= len(data) {
		return fuse.ReadResultData(nil), 0
	}
	return fuse.ReadResultData(data[off:end]), 0
}

func (f *realFile) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	existing, _ := os.ReadFile(f.filePath)

	end := int64(len(data)) + off
	if end > int64(len(existing)) {
		grown := make([]byte, end)
		copy(grown, existing)
		existing = grown
	}
	copy(existing[off:], data)

	if err := os.WriteFile(f.filePath, existing, 0o644); err != nil {
		return 0, syscall.EIO
	}
	return uint32(len(data)), 0
}

package fusefs

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// gitFile is a file backed by the GitHub repo tree. Reads go through
// Backend.AccessFile (RAM cache → disk if worktree exists → CDN). Writes
// trigger backend.WriteFile, which lazily provisions a worktree the first
// time it's called.
type gitFile struct {
	fs.Inode

	gitFS   *GitFS
	relPath string
	size    uint64
	mode    uint32

	mu           sync.Mutex
	cached       []byte // populated by AccessFile on first read
	materialized bool   // true after a write — disk is authoritative
}

var _ = (fs.NodeGetattrer)((*gitFile)(nil))
var _ = (fs.NodeOpener)((*gitFile)(nil))
var _ = (fs.NodeReader)((*gitFile)(nil))
var _ = (fs.NodeWriter)((*gitFile)(nil))
var _ = (fs.NodeSetattrer)((*gitFile)(nil))

func (f *gitFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.materialized {
		if info, err := os.Stat(f.diskPath()); err == nil {
			out.Size = uint64(info.Size())
			out.Mode = uint32(info.Mode())
			t := info.ModTime()
			out.SetTimes(&t, &t, &t)
			return 0
		}
	}

	if f.cached != nil {
		out.Size = uint64(len(f.cached))
	} else {
		out.Size = f.size
	}
	out.Mode = f.mode
	now := time.Now()
	out.SetTimes(&now, &now, &now)
	return 0
}

func (f *gitFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (f *gitFile) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.materialized {
		data, err := os.ReadFile(f.diskPath())
		if err != nil {
			return nil, syscall.EIO
		}
		return fuse.ReadResultData(slice(data, off, len(dest))), 0
	}

	if f.cached == nil {
		data, err := f.gitFS.backend.AccessFile(ctx, f.relPath)
		if err != nil {
			return nil, syscall.EIO
		}
		f.cached = data
	}
	return fuse.ReadResultData(slice(f.cached, off, len(dest))), 0
}

func (f *gitFile) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.materialized {
		// Seed worktree with the current bytes (or empty if we never read).
		if f.cached == nil {
			// Best-effort fetch; if the file truly doesn't exist on origin
			// we still create it locally below.
			if cur, err := f.gitFS.backend.AccessFile(ctx, f.relPath); err == nil {
				f.cached = cur
			}
		}
		seed := string(f.cached)
		if err := f.gitFS.backend.WriteFile(ctx, f.relPath, seed); err != nil {
			return 0, syscall.EIO
		}
		f.materialized = true
		f.cached = nil
	}

	full := f.diskPath()
	existing, _ := os.ReadFile(full)
	end := int64(len(data)) + off
	if end > int64(len(existing)) {
		grown := make([]byte, end)
		copy(grown, existing)
		existing = grown
	}
	copy(existing[off:], data)
	if err := os.WriteFile(full, existing, os.FileMode(f.mode)); err != nil {
		return 0, syscall.EIO
	}
	f.size = uint64(len(existing))
	return uint32(len(data)), 0
}

func (f *gitFile) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if sz, ok := in.GetSize(); ok {
		f.mu.Lock()
		defer f.mu.Unlock()

		if !f.materialized {
			seed := ""
			if f.cached != nil {
				seed = string(f.cached)
			}
			if err := f.gitFS.backend.WriteFile(ctx, f.relPath, seed); err != nil {
				return syscall.EIO
			}
			f.materialized = true
			f.cached = nil
		}
		if err := os.Truncate(f.diskPath(), int64(sz)); err != nil {
			return syscall.EIO
		}
		f.size = sz
	}
	out.Size = f.size
	out.Mode = f.mode
	return 0
}

// diskPath resolves the worktree-side absolute path for this file. Only
// meaningful after materialization (write or truncate).
func (f *gitFile) diskPath() string {
	wt := f.gitFS.backend.WorktreeDir()
	if wt == "" {
		return ""
	}
	return filepath.Join(wt, f.relPath)
}

func slice(data []byte, off int64, n int) []byte {
	if int(off) >= len(data) {
		return nil
	}
	end := int(off) + n
	if end > len(data) {
		end = len(data)
	}
	return data[off:end]
}

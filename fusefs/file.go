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
)

// gitFile is a file backed by a git blob. Reads are served from an in-memory
// cache (no disk I/O). The file is only materialized to the real worktree when
// a write or truncate occurs, preserving git compatibility for edits while
// keeping pure reads entirely in memory.
type gitFile struct {
	fs.Inode

	gitFS    *GitFS
	blobHash plumbing.Hash
	size     uint64
	mode     uint32
	relPath  string
	diskPath string

	mu           sync.Mutex
	cachedData   []byte // in-memory blob content; nil until first read
	materialized bool   // true once flushed to disk (write triggered)
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
		info, err := os.Stat(f.diskPath)
		if err == nil {
			out.Size = uint64(info.Size())
			out.Mode = uint32(info.Mode())
			modTime := info.ModTime()
			out.SetTimes(&modTime, &modTime, &modTime)
			return 0
		}
	}

	if f.cachedData != nil {
		out.Size = uint64(len(f.cachedData))
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
		data, err := os.ReadFile(f.diskPath)
		if err != nil {
			return nil, syscall.EIO
		}
		return fuse.ReadResultData(sliceData(data, off, len(dest))), 0
	}

	if err := f.ensureCached(); err != nil {
		return nil, syscall.EIO
	}

	return fuse.ReadResultData(sliceData(f.cachedData, off, len(dest))), 0
}

func (f *gitFile) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.ensureMaterialized(); err != nil {
		return 0, syscall.EIO
	}

	existing, _ := os.ReadFile(f.diskPath)

	end := int64(len(data)) + off
	if end > int64(len(existing)) {
		grown := make([]byte, end)
		copy(grown, existing)
		existing = grown
	}
	copy(existing[off:], data)

	if err := os.WriteFile(f.diskPath, existing, os.FileMode(f.mode)); err != nil {
		return 0, syscall.EIO
	}

	f.size = uint64(len(existing))
	return uint32(len(data)), 0
}

func (f *gitFile) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if sz, ok := in.GetSize(); ok {
		f.mu.Lock()
		defer f.mu.Unlock()

		if err := f.ensureMaterialized(); err != nil {
			return syscall.EIO
		}

		if err := os.Truncate(f.diskPath, int64(sz)); err != nil {
			return syscall.EIO
		}
		f.size = sz
	}

	out.Size = f.size
	out.Mode = f.mode
	return 0
}

// ensureCached fetches the blob from git into memory if not already present.
// Must be called with f.mu held.
func (f *gitFile) ensureCached() error {
	if f.cachedData != nil || f.materialized {
		return nil
	}

	data, err := f.gitFS.backend.ReadBlob(f.blobHash)
	if err != nil {
		log.Printf("cache %s: read blob: %v", f.relPath, err)
		return err
	}

	f.cachedData = data

	f.gitFS.mu.Lock()
	f.gitFS.blobsRead++
	f.gitFS.bytesRead += int64(len(data))
	f.gitFS.mu.Unlock()

	log.Printf("cached %s (%d bytes, in-memory)", f.relPath, len(data))
	return nil
}

// ensureMaterialized writes the file to the real worktree and updates
// sparse-checkout. Called only on write/truncate operations.
// Must be called with f.mu held.
func (f *gitFile) ensureMaterialized() error {
	if f.materialized {
		return nil
	}

	if err := f.ensureCached(); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(f.diskPath), 0o755); err != nil {
		log.Printf("materialize %s: mkdir: %v", f.relPath, err)
		return err
	}

	if err := os.WriteFile(f.diskPath, f.cachedData, os.FileMode(f.mode)); err != nil {
		log.Printf("materialize %s: write: %v", f.relPath, err)
		return err
	}

	if err := UpdateSparseCheckout(f.gitFS.gitDir, f.relPath); err != nil {
		log.Printf("materialize %s: sparse-checkout: %v", f.relPath, err)
	}

	f.materialized = true
	f.cachedData = nil // disk is authoritative now; free the memory

	f.gitFS.mu.Lock()
	f.gitFS.diskMaterializations++
	f.gitFS.mu.Unlock()

	log.Printf("materialized %s to disk (write triggered)", f.relPath)
	return nil
}

func sliceData(data []byte, off int64, destLen int) []byte {
	if int(off) >= len(data) {
		return nil
	}
	end := int(off) + destLen
	if end > len(data) {
		end = len(data)
	}
	return data[off:end]
}

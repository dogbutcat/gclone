// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

// FUSE main Fs

//go:build linux

package mount

import (
	"context"
	"sync"
	"syscall"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
	"github.com/dogbutcat/gclone/cmd/mountlib"
	"github.com/dogbutcat/gclone/vfs"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/log"
)

// FS represents the top level filing system
type FS struct {
	*vfs.VFS
	f                 fs.Fs
	opt               *mountlib.Options
	server            *fusefs.Server
	nodesMu           sync.Mutex // canonical wrapper publication; never held over VFS I/O
	kernelCache       bool       // full-cache data lifetime is controlled explicitly
	kernelContext     context.Context
	cancelKernelCache context.CancelFunc
}

// Check interface satisfied
var _ fusefs.FS = (*FS)(nil)

// NewFS makes a new FS
func NewFS(VFS *vfs.VFS, opt *mountlib.Options) *FS {
	ctx, cancel := context.WithCancel(context.Background())
	fsys := &FS{
		VFS:               VFS,
		f:                 VFS.Fs(),
		opt:               opt,
		kernelCache:       mountlib.UsesKernelCacheControl(VFS, opt),
		kernelContext:     ctx,
		cancelKernelCache: cancel,
	}
	return fsys
}

// Root returns the root node
func (f *FS) Root() (node fusefs.Node, err error) {
	defer log.Trace("", "")("node=%+v, err=%v", &node, &err)
	root, err := f.VFS.Root()
	if err != nil {
		return nil, translateError(err)
	}
	return f.node(root), nil
}

// node returns one FUSE wrapper per VFS lifetime. Different wrappers for the
// same VFS node would create different kernel lock/page-cache identities.
func (f *FS) node(node vfs.Node) fusefs.Node {
	f.nodesMu.Lock()
	defer f.nodesMu.Unlock()
	if cached, ok := node.Aux(f).(fusefs.Node); ok {
		return cached
	}
	var cached fusefs.Node
	switch n := node.(type) {
	case *vfs.File:
		cached = &File{File: n, fsys: f, kernelCache: mountlib.NewKernelCacheWithContext(f.kernelContext)}
	case *vfs.Dir:
		cached = &Dir{Dir: n, fsys: f}
	default:
		panic("bad type")
	}
	node.SetAux(f, cached)
	return cached
}

// Check interface satisfied
var _ fusefs.FSStatfser = (*FS)(nil)

// Statfs is called to obtain file system metadata.
// It should write that data to resp.
func (f *FS) Statfs(ctx context.Context, req *fuse.StatfsRequest, resp *fuse.StatfsResponse) (err error) {
	defer log.Trace("", "")("stat=%+v, err=%v", resp, &err)
	const blockSize = 4096
	total, _, free := f.VFS.Statfs()
	resp.Blocks = uint64(total) / blockSize // Total data blocks in file system.
	resp.Bfree = uint64(free) / blockSize   // Free blocks in file system.
	resp.Bavail = resp.Bfree                // Free blocks in file system if you're not root.
	resp.Files = 1e9                        // Total files in file system.
	resp.Ffree = 1e9                        // Free files in file system.
	resp.Bsize = blockSize                  // Block size
	resp.Namelen = 255                      // Maximum file name length?
	resp.Frsize = blockSize                 // Fragment size, smallest addressable data size in the file system.
	mountlib.ClipBlocks(&resp.Blocks)
	mountlib.ClipBlocks(&resp.Bfree)
	mountlib.ClipBlocks(&resp.Bavail)
	return nil
}

// Translate errors from mountlib
func translateError(err error) error {
	if err == nil {
		return nil
	}
	_, uErr := fserrors.Cause(err)
	switch uErr {
	case vfs.OK:
		return nil
	case vfs.ENOENT, fs.ErrorDirNotFound, fs.ErrorObjectNotFound:
		return fuse.Errno(syscall.ENOENT)
	case vfs.EEXIST, fs.ErrorDirExists:
		return fuse.Errno(syscall.EEXIST)
	case vfs.EPERM, fs.ErrorPermissionDenied:
		return fuse.Errno(syscall.EPERM)
	case vfs.ECLOSED:
		return fuse.Errno(syscall.EBADF)
	case vfs.ENOTEMPTY:
		return fuse.Errno(syscall.ENOTEMPTY)
	case vfs.ESPIPE:
		return fuse.Errno(syscall.ESPIPE)
	case vfs.EBADF:
		return fuse.Errno(syscall.EBADF)
	case vfs.EROFS:
		return fuse.Errno(syscall.EROFS)
	case vfs.ENOSYS, fs.ErrorNotImplemented:
		return syscall.ENOSYS
	case vfs.EINVAL:
		return fuse.Errno(syscall.EINVAL)
	case vfs.ELOOP:
		return fuse.Errno(syscall.ELOOP)
	case syscall.EAGAIN:
		return fuse.Errno(syscall.EAGAIN)
	case context.Canceled:
		return fuse.Errno(syscall.EINTR)
	case context.DeadlineExceeded:
		return fuse.Errno(syscall.ETIMEDOUT)
	}
	fs.Errorf(nil, "IO error: %v", err)
	return err
}

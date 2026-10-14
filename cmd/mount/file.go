// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build linux

package mount

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
	"github.com/dogbutcat/gclone/cmd/mountlib"
	"github.com/dogbutcat/gclone/vfs"
	"github.com/rclone/rclone/fs/log"
)

// File represents a file
type File struct {
	*vfs.File
	fsys        *FS
	kernelCache *mountlib.KernelCache
}

func (f *File) cacheControl() *mountlib.KernelCache {
	if !f.fsys.kernelCache {
		return nil
	}
	if entry := f.File.DirEntry(); entry != nil && entry.Size() < 0 {
		return nil
	}
	f.fsys.nodesMu.Lock()
	defer f.fsys.nodesMu.Unlock()
	if f.kernelCache == nil {
		f.kernelCache = mountlib.NewKernelCacheWithContext(f.fsys.kernelContext)
	}
	return f.kernelCache
}

// Check interface satisfied
var _ fusefs.Node = (*File)(nil)

// Attr fills out the attributes for the file
func (f *File) Attr(ctx context.Context, a *fuse.Attr) (err error) {
	defer log.Trace(f, "")("a=%+v, err=%v", a, &err)
	a.Valid = time.Duration(f.fsys.opt.AttrTimeout)
	// Keep one inode for this VFS lifetime across lookup, getattr and rename.
	// Bazil's pathname-derived fallback differs from a saved node after rename.
	a.Inode = f.File.Inode()
	modTime := f.File.ModTime()
	Size := uint64(f.File.Size())
	Blocks := (Size + 511) / 512
	a.Gid = f.VFS().Opt.GID
	a.Uid = f.VFS().Opt.UID
	a.Mode = f.File.Mode() &^ os.ModeAppend
	a.Size = Size
	a.Atime = modTime
	a.Mtime = modTime
	a.Ctime = modTime
	a.Blocks = Blocks
	return nil
}

// Check interface satisfied
var _ fusefs.NodeSetattrer = (*File)(nil)

// Setattr handles attribute changes from FUSE. Currently supports ModTime and Size only
func (f *File) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) (err error) {
	defer log.Trace(f, "a=%+v", req)("err=%v", &err)
	if !f.VFS().Opt.NoModTime {
		if req.Valid.Mtime() {
			err = f.File.SetModTime(req.Mtime)
		} else if req.Valid.MtimeNow() {
			err = f.File.SetModTime(time.Now())
		}
	}
	if req.Valid.Size() {
		if control := f.cacheControl(); control != nil {
			err = f.File.TruncateForKernel(int64(req.Size), control.Owner())
		} else {
			err = f.File.Truncate(int64(req.Size))
		}
	}
	return translateError(err)
}

// Check interface satisfied
var _ fusefs.NodeOpener = (*File)(nil)

// Open the file for read or write
func (f *File) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fh fusefs.Handle, err error) {
	defer log.Trace(f, "flags=%v", req.Flags)("fh=%v, err=%v", &fh, &err)

	// fuse flags are based off syscall flags as are os flags, so
	// should be compatible
	control := f.cacheControl()
	var handle vfs.Handle
	if control != nil {
		handle, err = f.File.OpenForKernel(int(req.Flags), control.Owner())
	} else {
		handle, err = f.File.Open(int(req.Flags))
	}
	if err != nil {
		return nil, translateError(err)
	}
	if err = mountlib.PrepareHandle(handle); err != nil {
		return nil, translateError(err)
	}

	// If size unknown then use direct io to read
	if entry := handle.Node().DirEntry(); entry != nil && entry.Size() < 0 {
		resp.Flags |= fuse.OpenDirectIO
	}
	if f.fsys.opt.DirectIO {
		resp.Flags |= fuse.OpenDirectIO
	}
	if control != nil && resp.Flags&fuse.OpenDirectIO == 0 {
		keep, prepareErr := control.Prepare(ctx, handle, func() error {
			if f.fsys.server == nil {
				return nil
			} // adapter-only tests have no kernel
			err := f.fsys.server.InvalidateNodeData(f)
			if errors.Is(err, fuse.ErrNotCached) {
				return nil
			}
			return err
		})
		if prepareErr != nil {
			return nil, translateError(errors.Join(prepareErr, handle.Close()))
		}
		if keep {
			resp.Flags |= fuse.OpenKeepCache
		}
	}

	return &FileHandle{Handle: handle}, nil
}

// Check interface satisfied
var _ fusefs.NodeFsyncer = (*File)(nil)

// Fsync the file
//
// Descriptor requests use FileHandle.Fsync; retain the legacy node fallback.
func (f *File) Fsync(ctx context.Context, req *fuse.FsyncRequest) (err error) {
	defer log.Trace(f, "")("err=%v", &err)
	return nil
}

// Getxattr gets an extended attribute by the given name from the
// node.
//
// If there is no xattr by that name, returns fuse.ErrNoXattr.
func (f *File) Getxattr(ctx context.Context, req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse) error {
	return syscall.ENOSYS // we never implement this
}

var _ fusefs.NodeGetxattrer = (*File)(nil)

// Listxattr lists the extended attributes recorded for the node.
func (f *File) Listxattr(ctx context.Context, req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse) error {
	return syscall.ENOSYS // we never implement this
}

var _ fusefs.NodeListxattrer = (*File)(nil)

// Setxattr sets an extended attribute with the given name and
// value for the node.
func (f *File) Setxattr(ctx context.Context, req *fuse.SetxattrRequest) error {
	return syscall.ENOSYS // we never implement this
}

var _ fusefs.NodeSetxattrer = (*File)(nil)

// Removexattr removes an extended attribute for the name.
//
// If there is no xattr by that name, returns fuse.ErrNoXattr.
func (f *File) Removexattr(ctx context.Context, req *fuse.RemovexattrRequest) error {
	return syscall.ENOSYS // we never implement this
}

var _ fusefs.NodeRemovexattrer = (*File)(nil)

var _ fusefs.NodeReadlinker = (*File)(nil)

// Readlink read symbolic link target.
func (f *File) Readlink(ctx context.Context, req *fuse.ReadlinkRequest) (ret string, err error) {
	defer log.Trace(f, "")("ret=%v, err=%v", &ret, &err)
	return f.VFS().Readlink(f.Path())
}

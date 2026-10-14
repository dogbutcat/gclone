// Verify canonical FUSE nodes and the local-cache fsync contract on Linux.
//go:build linux

package mount

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bazil.org/fuse"
	"github.com/dogbutcat/gclone/cmd/mountlib"
	"github.com/dogbutcat/gclone/vfs"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/stretchr/testify/require"
)

func mountAdapterFixture(t *testing.T) (*Dir, string) {
	t.Helper()
	cache := t.TempDir()
	previous := config.GetCacheDir()
	require.NoError(t, config.SetCacheDir(cache))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(previous)) })
	remote, err := fs.NewFs(context.Background(), t.TempDir())
	require.NoError(t, err)
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.CachePollInterval, opt.HandleCaching, opt.WriteBack = 0, 0, 0
	view := vfs.New(context.Background(), remote, &opt)
	t.Cleanup(func() { view.WaitForWriters(5 * time.Second); view.Shutdown(); require.NoError(t, view.CleanUp()) })
	root, err := view.Root()
	require.NoError(t, err)
	return &Dir{Dir: root, fsys: NewFS(view, &mountlib.Opt)}, cache
}

func TestCreateExistingPreservesKernelNode(t *testing.T) {
	d, _ := mountAdapterFixture(t)
	ctx := context.Background()
	node, handle, err := d.Create(ctx, &fuse.CreateRequest{Name: "shared", Flags: fuse.OpenFlags(os.O_RDWR)}, &fuse.CreateResponse{})
	require.NoError(t, err)
	defer handle.(*FileHandle).Release(ctx, &fuse.ReleaseRequest{})
	lookup, err := d.Lookup(ctx, &fuse.LookupRequest{Name: "shared"}, &fuse.LookupResponse{})
	require.NoError(t, err)
	require.Same(t, node, lookup)
	again, second, err := d.Create(ctx, &fuse.CreateRequest{Name: "shared", Flags: fuse.OpenFlags(os.O_RDWR)}, &fuse.CreateResponse{})
	require.NoError(t, err)
	defer second.(*FileHandle).Release(ctx, &fuse.ReleaseRequest{})
	require.Same(t, node, again, "a second open through CREATE must use the same kernel lock and page-cache identity")
}

func TestFsyncPersistsCacheMetadataAndReportsFailure(t *testing.T) {
	d, cache := mountAdapterFixture(t)
	ctx := context.Background()
	_, raw, err := d.Create(ctx, &fuse.CreateRequest{Name: "sync-data", Flags: fuse.OpenFlags(os.O_RDWR)}, &fuse.CreateResponse{})
	require.NoError(t, err)
	handle := raw.(*FileHandle)
	defer handle.Release(ctx, &fuse.ReleaseRequest{})
	_, err = handle.Handle.Write([]byte("committed cache bytes"))
	require.NoError(t, err)
	require.NoError(t, handle.Fsync(ctx, &fuse.FsyncRequest{}))
	var metadata string
	require.NoError(t, filepath.WalkDir(filepath.Join(cache, "vfsMeta"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.Name() == "sync-data" && !entry.IsDir() {
			metadata = path
		}
		return err
	}))
	require.NotEmpty(t, metadata)
	data, err := os.ReadFile(metadata)
	require.NoError(t, err)
	var info struct{ Size int64 }
	require.NoError(t, json.Unmarshal(data, &info))
	require.Equal(t, int64(len("committed cache bytes")), info.Size)
	// A failed sidecar save must not be acknowledged as a successful fsync.
	require.NoError(t, os.Rename(metadata, metadata+".saved"))
	require.NoError(t, os.Mkdir(metadata, 0700))
	err = handle.Fsync(ctx, &fuse.FsyncRequest{})
	require.NoError(t, os.Remove(metadata))
	require.NoError(t, os.Rename(metadata+".saved", metadata))
	require.Error(t, err)
}

// Check that CREATE returns attributes from the successfully prepared handle.
//go:build linux || (darwin && amd64)

package mount2

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/cmd/mountlib"
	"github.com/dogbutcat/gclone/vfs"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/stretchr/testify/require"
)

func TestCreateConcurrentTruncateReturnsAcceptedAttributes(t *testing.T) {
	ctx := context.Background()
	backing := t.TempDir()
	previous := config.GetCacheDir()
	require.NoError(t, config.SetCacheDir(t.TempDir()))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(previous)) })
	remote, err := fs.NewFs(ctx, backing)
	require.NoError(t, err)
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.CachePollInterval, opt.HandleCaching, opt.WriteBack = 0, 0, 0
	view := vfs.New(ctx, remote, &opt)
	t.Cleanup(func() {
		view.WaitForWriters(time.Second)
		view.Shutdown()
		require.NoError(t, view.CleanUp())
	})
	fsys := NewFS(view, &mountlib.Opt)
	defer fsys.cancelKernelCache()
	root, err := fsys.Root()
	require.NoError(t, err)
	raw := fusefs.NewNodeFS(root, &fusefs.Options{})
	var absent fuse.EntryOut
	require.Equal(t, fuse.ENOENT, raw.Lookup(nil, &fuse.InHeader{NodeId: 1}, "data", &absent))
	// Another VFS client creates the name after the kernel's negative lookup.
	creator, err := view.OpenFile("data", os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	_, err = creator.Write(make([]byte, 8192))
	require.NoError(t, err)
	require.NoError(t, creator.Close())
	var out fuse.CreateOut
	in := fuse.CreateIn{InHeader: fuse.InHeader{NodeId: 1}, Flags: uint32(os.O_RDWR | os.O_CREATE | os.O_TRUNC), Mode: 0600}
	status := raw.Create(nil, &in, "data", &out)
	require.Equal(t, fuse.OK, status)
	defer raw.Release(nil, &fuse.ReleaseIn{InHeader: fuse.InHeader{NodeId: out.NodeId}, Fh: out.Fh})
	file, err := view.Stat("data")
	require.NoError(t, err)
	require.Equal(t, int64(0), file.Size(), "accepted VFS truncation must have completed")
	t.Logf("accepted size=%d CREATE response size=%d inode=%d", file.Size(), out.Attr.Size, out.Attr.Ino)
	require.Equal(t, uint64(file.Size()), out.Attr.Size, "CREATE response must not republish the pre-truncate size")
}

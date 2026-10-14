// Exercise CREATE after a negative lookup races an ordinary VFS creator.
//go:build linux

package mount2

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/cmd/mountlib"
	"github.com/dogbutcat/gclone/vfs"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type createLookupBarrier struct {
	fuse.RawFileSystem
	entered, release chan struct{}
	once             sync.Once
	creates          atomic.Int32
	size             atomic.Uint64
}

func (b *createLookupBarrier) Lookup(cancel <-chan struct{}, h *fuse.InHeader, name string, out *fuse.EntryOut) fuse.Status {
	status := b.RawFileSystem.Lookup(cancel, h, name, out)
	if name == "data" && status == fuse.ENOENT {
		b.once.Do(func() { close(b.entered); <-b.release })
	}
	return status
}

func (b *createLookupBarrier) Create(cancel <-chan struct{}, in *fuse.CreateIn, name string, out *fuse.CreateOut) fuse.Status {
	b.creates.Add(1)
	status := b.RawFileSystem.Create(cancel, in, name, out)
	if status == fuse.OK {
		b.size.Store(out.Attr.Size)
	}
	return status
}

func TestMountedCreateAfterConcurrentCreator(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("requires Linux FUSE")
	}
	ctx := context.Background()
	backing, point := t.TempDir(), t.TempDir()
	previous := config.GetCacheDir()
	require.NoError(t, config.SetCacheDir(t.TempDir()))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(previous)) })
	remote, err := fs.NewFs(ctx, backing)
	require.NoError(t, err)
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.CachePollInterval, opt.HandleCaching, opt.WriteBack = 0, 0, 0
	view := vfs.New(ctx, remote, &opt)
	mountOpt := mountlib.Opt
	fsys := NewFS(view, &mountOpt)
	root, err := fsys.Root()
	require.NoError(t, err)
	options := fusefs.Options{MountOptions: *mountOptions(fsys, remote, &mountOpt), EntryTimeout: (*time.Duration)(&mountOpt.AttrTimeout), AttrTimeout: (*time.Duration)(&mountOpt.AttrTimeout)}
	barrier := &createLookupBarrier{RawFileSystem: fusefs.NewNodeFS(root, &options), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(barrier.release) }) }
	server, err := fuse.NewServer(barrier, point, &options.MountOptions)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() { server.Serve(); close(done) }()
	t.Cleanup(func() {
		release()
		require.NoError(t, server.Unmount())
		<-done
		fsys.cancelKernelCache()
		view.Shutdown()
		require.NoError(t, view.CleanUp())
	})
	require.NoError(t, server.WaitMount())
	type opened struct {
		fd  int
		err error
	}
	result := make(chan opened, 1)
	go func() {
		fd, err := unix.Open(filepath.Join(point, "data"), unix.O_CREAT|unix.O_RDWR|unix.O_TRUNC, 0600)
		result <- opened{fd, err}
	}()
	defer release()
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("negative LOOKUP barrier was not reached")
	}
	creator, err := view.OpenFile("data", os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	_, err = creator.Write(make([]byte, 8192))
	require.NoError(t, err)
	require.NoError(t, creator.Close())
	release()
	var created opened
	select {
	case created = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("CREATE did not finish")
	}
	require.NoError(t, created.err)
	defer unix.Close(created.fd)
	require.Equal(t, int32(1), barrier.creates.Load(), "the kernel must exercise CREATE, not a later OPEN")
	require.Zero(t, barrier.size.Load(), "CREATE must publish its post-truncate attributes")
	var stat unix.Stat_t
	require.NoError(t, unix.Fstat(created.fd, &stat))
	require.Zero(t, stat.Size)
	n, err := unix.Pread(created.fd, make([]byte, 1), 0)
	require.NoError(t, err)
	require.Zero(t, n)
}

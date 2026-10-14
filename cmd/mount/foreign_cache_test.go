// Verify that another mount or VFS writer invalidates a held kernel reader.
//go:build linux

package mount

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
	"github.com/dogbutcat/gclone/cmd/mountlib"
	"github.com/dogbutcat/gclone/vfs"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestMountForeignWriteRetiresHeldReader(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("requires Linux FUSE")
	}
	for _, readerIndex := range []int{0, 1} {
		for _, origin := range []string{"mount", "vfs"} {
			t.Run(fmt.Sprintf("reader%d/%s", readerIndex, origin), func(t *testing.T) {
				backing, cache := t.TempDir(), t.TempDir()
				old, next := bytes.Repeat([]byte{'O'}, 4096), bytes.Repeat([]byte{'N'}, 4096)
				require.NoError(t, os.WriteFile(filepath.Join(backing, "data"), old, 0600))
				previous := config.GetCacheDir()
				require.NoError(t, config.SetCacheDir(cache))
				t.Cleanup(func() { require.NoError(t, config.SetCacheDir(previous)) })
				remote, err := fs.NewFs(context.Background(), backing)
				require.NoError(t, err)
				opt := vfscommon.Opt
				opt.CacheMode = vfscommon.CacheModeFull
				opt.CachePollInterval, opt.HandleCaching, opt.WriteBack = 0, 0, 0
				view := vfs.New(context.Background(), remote, &opt)
				t.Cleanup(func() { view.WaitForWriters(time.Second); view.Shutdown(); require.NoError(t, view.CleanUp()) })
				points := []string{t.TempDir(), t.TempDir()}
				for _, point := range points {
					mountOpt := mountlib.Opt
					conn, err := fuse.Mount(point, mountOptions(view, "gclone-foreign-view-probe", &mountOpt)...)
					require.NoError(t, err)
					server := NewFS(view, &mountOpt)
					server.server = fusefs.New(conn, nil)
					done := make(chan error, 1)
					go func() { done <- server.server.Serve(server) }()
					t.Cleanup(func() {
						require.NoError(t, fuse.Unmount(point))
						require.NoError(t, conn.Close())
						require.NoError(t, <-done)
						server.cancelKernelCache()
					})
				}
				reader, err := unix.Open(filepath.Join(points[readerIndex], "data"), unix.O_RDONLY, 0)
				require.NoError(t, err)
				defer unix.Close(reader)
				got := make([]byte, len(old))
				_, err = unix.Pread(reader, got, 0)
				require.NoError(t, err)
				require.Equal(t, old, got)
				if origin == "mount" {
					writer, err := unix.Open(filepath.Join(points[1-readerIndex], "data"), unix.O_RDWR, 0)
					require.NoError(t, err)
					defer unix.Close(writer)
					n, err := unix.Pwrite(writer, next, 0)
					require.NoError(t, err)
					require.Equal(t, len(next), n)
				} else {
					writer, err := view.OpenFile("data", os.O_RDWR, 0600)
					require.NoError(t, err)
					_, err = writer.WriteAt(next, 0)
					require.NoError(t, err)
					require.NoError(t, writer.Close())
				}
				// No OPEN/LOOKUP/GETATTR between the write acknowledgement and read.
				_, err = unix.Pread(reader, got, 0)
				require.NoError(t, err)
				require.Equal(t, next, got)
			})
		}
	}
}

func TestMountDelayedReadCannotRepopulateRetiredView(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("requires Linux FUSE")
	}
	backing, point, cache := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(backing, "data")
	old, next := bytes.Repeat([]byte{'O'}, 8192), bytes.Repeat([]byte{'N'}, 8192)
	when := time.Unix(1700000000, 0)
	require.NoError(t, os.WriteFile(path, old, 0600))
	require.NoError(t, os.Chtimes(path, when, when))
	previous := config.GetCacheDir()
	require.NoError(t, config.SetCacheDir(cache))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(previous)) })
	remote, err := fs.NewFs(context.Background(), backing)
	require.NoError(t, err)
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.DirCacheTime, opt.CachePollInterval, opt.HandleCaching, opt.WriteBack = 0, 0, 0, 0
	view := vfs.New(context.Background(), remote, &opt)
	mountOpt := mountlib.Opt
	mountOpt.MaxReadAhead = 0
	conn, err := fuse.Mount(point, mountOptions(view, "gclone-delayed-read-probe", &mountOpt)...)
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := NewFS(view, &mountOpt)
	server.server = fusefs.New(conn, &fusefs.Config{Debug: func(msg interface{}) {
		// Server.done logs after Handle.Read populated its detached response,
		// before ReadRequest.Respond. Retain no message beyond this callback.
		if strings.Contains(fmt.Sprint(msg), "] Read ") && armed.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
	}})
	served := make(chan error, 1)
	go func() { served <- server.server.Serve(server) }()
	t.Cleanup(func() {
		unblock()
		require.NoError(t, fuse.Unmount(point))
		require.NoError(t, conn.Close())
		require.NoError(t, <-served)
		server.cancelKernelCache()
		view.Shutdown()
		require.NoError(t, view.CleanUp())
	})
	a, err := unix.Open(filepath.Join(point, "data"), unix.O_RDONLY, 0)
	require.NoError(t, err)
	defer unix.Close(a)
	warm := make([]byte, 4096)
	_, err = unix.Pread(a, warm, 0)
	require.NoError(t, err)
	require.Equal(t, old[:4096], warm)
	armed.Store(true)
	late := make([]byte, 4096)
	read := make(chan error, 1)
	go func() { _, err := unix.Pread(a, late, 4096); read <- err }()
	defer unblock()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("READ response did not reach its post-copy barrier")
	}
	require.NoError(t, os.WriteFile(path, next, 0600))
	require.NoError(t, os.Chtimes(path, when, when))
	root, err := view.Root()
	require.NoError(t, err)
	_, err = root.ReadDirAll()
	require.NoError(t, err)
	type opened struct {
		fd  int
		err error
	}
	accepted := make(chan opened, 1)
	go func() {
		fd, err := unix.Open(filepath.Join(point, "data"), unix.O_RDONLY, 0)
		accepted <- opened{fd, err}
	}()
	var b opened
	early := false
	select {
	case b = <-accepted:
		early = true
	case <-time.After(30 * time.Millisecond):
	}
	unblock()
	require.NoError(t, <-read)
	if !early {
		b = <-accepted
	}
	require.NoError(t, b.err)
	defer unix.Close(b.fd)
	// The pending read may complete from its old response or be refetched by
	// the kernel. After the new OPEN is acknowledged, only new bytes are valid.
	require.True(t, bytes.Equal(old[4096:], late) || bytes.Equal(next[4096:], late))
	_, err = unix.Pread(a, warm, 4096)
	require.NoError(t, err)
	require.Equal(t, next[4096:], warm, "a completed old READ repopulated data after replacement acknowledgement")
}

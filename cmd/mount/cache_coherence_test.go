// Exercise acknowledged writes through the real mount adapter and Linux cache.
//go:build linux

package mount

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
	"github.com/dogbutcat/gclone/cmd/mountlib"
	"github.com/dogbutcat/gclone/vfs"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestMountAcknowledgedWriteSurvivesConcurrentOpen(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("requires Linux FUSE")
	}
	for _, noModTime := range []bool{false, true} {
		name := "mtime"
		if noModTime {
			name = "no-modtime"
		}
		t.Run(name, func(t *testing.T) {
			backing, point, cache := t.TempDir(), t.TempDir(), t.TempDir()
			original, updated := bytes.Repeat([]byte{'O'}, 4096), bytes.Repeat([]byte{'N'}, 4096)
			require.NoError(t, os.WriteFile(filepath.Join(backing, "data"), original, 0600))
			previousCache := config.GetCacheDir()
			require.NoError(t, config.SetCacheDir(cache))
			t.Cleanup(func() { require.NoError(t, config.SetCacheDir(previousCache)) })
			remote, err := fs.NewFs(context.Background(), backing)
			require.NoError(t, err)
			opt := vfscommon.Opt
			opt.CacheMode, opt.NoModTime = vfscommon.CacheModeFull, noModTime
			opt.CachePollInterval, opt.HandleCaching, opt.WriteBack = 0, 0, 0
			view := vfs.New(context.Background(), remote, &opt)
			mountOpt := mountlib.Opt
			conn, err := fuse.Mount(point, mountOptions(view, "gclone-coherence-probe", &mountOpt)...)
			require.NoError(t, err)
			entered, released := make(chan struct{}), make(chan struct{})
			var gate, releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(released) }) }
			server := NewFS(view, &mountOpt)
			server.server = fusefs.New(conn, &fusefs.Config{WithContext: func(ctx context.Context, req fuse.Request) context.Context {
				if write, ok := req.(*fuse.WriteRequest); ok && bytes.Equal(write.Data, updated) {
					gate.Do(func() { close(entered); <-released })
				}
				return ctx
			}})
			served := make(chan error, 1)
			go func() { served <- server.server.Serve(server) }()
			t.Cleanup(func() {
				release()
				view.WaitForWriters(10 * time.Second)
				require.NoError(t, fuse.Unmount(point))
				require.NoError(t, conn.Close())
				require.NoError(t, <-served)
				view.Shutdown()
				require.NoError(t, view.CleanUp())
			})
			// os.Open would try epoll on its own FUSE server while holding the
			// Go netpoll lock. Use raw client descriptors in this in-process test.
			a, err := unix.Open(filepath.Join(point, "data"), unix.O_RDWR, 0)
			require.NoError(t, err)
			defer unix.Close(a)
			warm := make([]byte, len(original))
			n, err := unix.Pread(a, warm, 0)
			require.NoError(t, err)
			require.Equal(t, len(warm), n)
			require.Equal(t, original, warm)
			var beforeStat unix.Stat_t
			require.NoError(t, unix.Fstat(a, &beforeStat))
			written := make(chan error, 1)
			go func() { _, err := unix.Pwrite(a, updated, 0); written <- err }()
			defer release()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("write did not reach barrier")
			}
			b := -1
			before := make([]byte, len(original))
			read := make(chan error, 1)
			go func() {
				var err error
				b, err = unix.Open(filepath.Join(point, "data"), unix.O_RDONLY, 0)
				if err == nil {
					_, err = unix.Pread(b, before, 0)
				}
				read <- err
			}()
			select {
			case err := <-read:
				require.NoError(t, err)
			case <-time.After(3 * time.Second):
				release()
				<-written
				<-read
				if b >= 0 {
					unix.Close(b)
				}
				t.Fatal("kernel serialized the probe; intended ordering was not reproduced")
			}
			defer unix.Close(b)
			release()
			require.NoError(t, <-written)
			after := make([]byte, len(updated))
			n, err = unix.Pread(b, after, 0)
			require.NoError(t, err)
			require.Equal(t, len(after), n)
			var cacheFile string
			require.NoError(t, filepath.WalkDir(filepath.Join(cache, "vfs"), func(p string, entry os.DirEntry, err error) error {
				if err == nil && !entry.IsDir() && entry.Name() == "data" {
					cacheFile = p
				}
				return err
			}))
			require.NotEmpty(t, cacheFile)
			physical, err := os.ReadFile(cacheFile)
			require.NoError(t, err)
			require.Equal(t, updated, physical)
			t.Logf("before=%q after=%q cache=%q", before[:8], after[:8], physical[:8])
			require.Equal(t, updated, after, "acknowledged write is hidden by kernel cache")
			if noModTime {
				var afterStat unix.Stat_t
				require.NoError(t, unix.Fstat(a, &afterStat))
				require.Equal(t, beforeStat.Mtim, afterStat.Mtim, "constant-mtime control changed its timestamp")
			}
		})
	}
}

func TestMountExternalReplacementInvalidatesKernelData(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("requires Linux FUSE")
	}
	for _, mode := range []string{"open", "truncate", "cancelled-open", "failed-notify"} {
		for _, size := range []int{4096, 8192} {
			t.Run(fmt.Sprintf("%s/%d", mode, size), func(t *testing.T) {
				backing, point, cache := t.TempDir(), t.TempDir(), t.TempDir()
				path := filepath.Join(backing, "data")
				original, updated := bytes.Repeat([]byte{'O'}, 4096), bytes.Repeat([]byte{'N'}, size)
				when := time.Unix(1700000000, 0)
				require.NoError(t, os.WriteFile(path, original, 0600))
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
				conn, err := fuse.Mount(point, mountOptions(view, "gclone-external-view-probe", &mountOpt)...)
				require.NoError(t, err)
				server := NewFS(view, &mountOpt)
				server.server = fusefs.New(conn, nil)
				served := make(chan error, 1)
				go func() { served <- server.server.Serve(server) }()
				t.Cleanup(func() {
					view.WaitForWriters(time.Second)
					require.NoError(t, fuse.Unmount(point))
					require.NoError(t, conn.Close())
					require.NoError(t, <-served)
					view.Shutdown()
					require.NoError(t, view.CleanUp())
				})
				var descriptors []int
				defer func() {
					for _, fd := range descriptors {
						_ = unix.Close(fd)
					}
				}()
				open := func(flags int) int {
					t.Helper()
					fd, err := unix.Open(filepath.Join(point, "data"), flags, 0)
					require.NoError(t, err)
					descriptors = append(descriptors, fd)
					return fd
				}
				a := open(unix.O_RDONLY)
				buf := make([]byte, len(original))
				_, err = unix.Pread(a, buf, 0)
				require.NoError(t, err)
				require.Equal(t, original, buf)
				var oldStat unix.Stat_t
				require.NoError(t, unix.Fstat(a, &oldStat))
				require.NoError(t, os.WriteFile(path, updated, 0600))
				require.NoError(t, os.Chtimes(path, when, when)) // Same size/mtime case requires content identity.
				root, err := view.Root()
				require.NoError(t, err)
				_, err = root.ReadDirAll()
				require.NoError(t, err)
				b := a
				switch mode {
				case "open":
					b = open(unix.O_RDONLY)
				case "truncate":
					require.NoError(t, unix.Truncate(filepath.Join(point, "data"), int64(size+4096)))
					updated = append(updated, make([]byte, 4096)...)
				case "cancelled-open", "failed-notify":
					node, err := view.Stat("data")
					require.NoError(t, err)
					file := server.node(node).(*File)
					control := file.cacheControl()
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					release := make(chan struct{})
					var releaseOnce sync.Once
					finish := func() { releaseOnce.Do(func() { close(release) }) }
					defer finish()
					if mode == "cancelled-open" {
						cancel()
					} else {
						var calls atomic.Int32
						control.SetNotifier(func() error {
							if calls.Add(1) == 1 {
								return errors.New("injected notification failure")
							}
							<-release
							return server.server.InvalidateNodeData(file)
						})
					}
					_, err = file.Open(ctx, &fuse.OpenRequest{Flags: fuse.OpenReadOnly}, &fuse.OpenResponse{})
					require.Error(t, err)
					finish()
					require.Eventually(t, func() bool { return control.Wait(context.Background()) == nil }, 3*time.Second, time.Millisecond)
				}
				var newStat unix.Stat_t
				require.NoError(t, unix.Fstat(b, &newStat))
				require.Equal(t, oldStat.Ino, newStat.Ino)
				tail := make([]byte, 1)
				wantSize := len(updated)
				n, err := unix.Pread(b, tail, int64(wantSize-1))
				require.NoError(t, err)
				require.Equal(t, 1, n)
				require.Equal(t, updated[wantSize-1], tail[0])
				buf = make([]byte, wantSize)
				n, err = unix.Pread(b, buf, 0)
				require.NoError(t, err)
				require.Equal(t, wantSize, n)
				require.Equal(t, updated, buf)
				writer := open(unix.O_RDWR)
				_, err = unix.Pwrite(writer, []byte("EDIT"), 0)
				require.NoError(t, err)
				require.NoError(t, unix.Fsync(writer))
				for _, fd := range descriptors {
					require.NoError(t, unix.Close(fd))
				}
				descriptors = nil
				copy(updated, "EDIT")
				require.Eventually(t, func() bool { data, err := os.ReadFile(path); return err == nil && bytes.Equal(data, updated) }, 5*time.Second, 10*time.Millisecond, "replacement lost its named writeback cache")
			})
		}
	}
}

// Preserve metadata with an accepted descriptor across unlink and replacement.
package vfs

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

func TestRetiredHandleMetadata(t *testing.T) {
	for _, origin := range []string{"new", "uploaded"} {
		for _, retirement := range []string{"unlink", "replace"} {
			t.Run(origin+"/"+retirement, func(t *testing.T) {
				backing := t.TempDir()
				if origin == "uploaded" {
					require.NoError(t, os.WriteFile(filepath.Join(backing, "target"), []byte("old data"), 0600))
				}
				remote, err := fs.NewFs(context.Background(), backing)
				require.NoError(t, err)
				opt := vfscommon.Opt
				opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, fs.Duration(time.Hour)
				opt.CachePollInterval, opt.HandleCaching = 0, 0
				v := New(context.Background(), remote, &opt)
				t.Cleanup(func() { v.Shutdown(); require.NoError(t, v.CleanUp()) })
				writer, err := v.OpenFile("target", os.O_CREATE|os.O_RDWR, 0600)
				require.NoError(t, err)
				defer writer.Close()
				_, err = writer.WriteAt([]byte("old data"), 0)
				require.NoError(t, err)
				reader, err := v.OpenFile("target", os.O_RDONLY, 0)
				require.NoError(t, err)
				defer reader.Close()
				require.NoError(t, reader.(*RWFileHandle).Prepare())
				before, inode := reader.Node().ModTime(), reader.Node().Inode()
				name := "temporary"
				if retirement == "unlink" {
					require.NoError(t, writer.Node().Remove())
					name = "target"
				}
				fresh, err := v.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
				require.NoError(t, err)
				defer fresh.Close()
				_, err = fresh.WriteAt([]byte("new data"), 0)
				require.NoError(t, err)
				if retirement == "replace" {
					require.NoError(t, v.Rename(name, "target"))
				}
				defer func() { require.NoError(t, fresh.Node().Remove()) }()
				freshTime := fresh.Node().ModTime()
				require.Equal(t, inode, reader.Node().Inode())
				require.True(t, before.Equal(reader.Node().ModTime()), "retirement changed the old timestamp")
				time.Sleep(time.Millisecond)
				require.True(t, before.Equal(reader.Node().ModTime()), "stat changed the old timestamp")
				oldBytes := make([]byte, 8)
				_, err = reader.ReadAt(oldBytes, 0)
				require.NoError(t, err)
				require.Equal(t, "old data", string(oldBytes))
				_, err = writer.WriteAt([]byte("OLD"), 0)
				require.NoError(t, err)
				written := reader.Node().ModTime()
				require.True(t, written.After(before), "old write did not advance its own timestamp")
				time.Sleep(time.Millisecond)
				require.NoError(t, writer.Truncate(4))
				truncated := reader.Node().ModTime()
				require.True(t, truncated.After(written), "old truncate did not advance its own timestamp")
				require.True(t, truncated.Equal(reader.Node().ModTime()))
				require.Equal(t, int64(4), reader.Node().Size())
				require.True(t, freshTime.Equal(fresh.Node().ModTime()), "old descriptor changed replacement metadata")
				newBytes := make([]byte, 8)
				_, err = fresh.ReadAt(newBytes, 0)
				require.NoError(t, err)
				require.Equal(t, "new data", string(newBytes))
			})
		}
	}
}

func TestConcurrentModTimeAcrossDirectoryRename(t *testing.T) {
	backing := t.TempDir()
	for _, name := range []string{"a", "b"} {
		require.NoError(t, os.Mkdir(filepath.Join(backing, name), 0700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(backing, "a", "data"), []byte("stable bytes"), 0600))
	remote, err := fs.NewFs(context.Background(), backing)
	require.NoError(t, err)
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.CachePollInterval, opt.HandleCaching, opt.WriteBack = 0, 0, 0
	view := New(context.Background(), remote, &opt)
	t.Cleanup(func() { view.Shutdown(); require.NoError(t, view.CleanUp()) })
	handle, err := view.OpenFile("a/data", os.O_RDONLY, 0600)
	require.NoError(t, err)
	require.NoError(t, handle.(*RWFileHandle).Prepare())
	defer handle.Close()
	file := handle.Node()
	before := file.ModTime()
	stop := make(chan struct{})
	bad := make(chan time.Time, 1)
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got := file.ModTime(); !got.Equal(before) {
					select {
					case bad <- got:
					default:
					}
					return
				}
			}
		}()
	}
	var once sync.Once
	finish := func() { once.Do(func() { close(stop) }); readers.Wait() }
	defer finish()
	for range 50 {
		require.NoError(t, view.Rename("a/data", "b/data"))
		require.NoError(t, view.Rename("b/data", "a/data"))
	}
	finish()
	select {
	case got := <-bad:
		t.Fatalf("rename changed held-descriptor mtime: %v -> %v", before, got)
	default:
	}
}

type metadataWithoutModTime struct{ fs.Fs }

func (metadataWithoutModTime) Precision() time.Duration { return fs.ModTimeNotSupported }

func TestConcurrentVirtualModTimePublication(t *testing.T) {
	backing := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(backing, "data"), []byte("stable bytes"), 0600))
	remote, err := fs.NewFs(context.Background(), backing)
	require.NoError(t, err)
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeOff
	view := New(context.Background(), metadataWithoutModTime{remote}, &opt)
	t.Cleanup(func() { view.Shutdown() })
	node, err := view.Stat("data")
	require.NoError(t, err)
	file := node.(*File)
	object := file.getObject()
	before := file.ModTime()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 1000 {
				if got := file.ModTime(); !got.Equal(before) {
					t.Errorf("virtual mtime changed: %v -> %v", before, got)
					return
				}
			}
		}()
	}
	close(start)
	for range 100 {
		file.setObjectNoUpdate(object, time.Time{})
		runtime.Gosched()
	}
	wg.Wait()
}

func TestConcurrentStatAcrossRetirement(t *testing.T) {
	for _, retirement := range []string{"unlink", "replace"} {
		t.Run(retirement, func(t *testing.T) {
			remote, err := fs.NewFs(context.Background(), t.TempDir())
			require.NoError(t, err)
			opt := vfscommon.Opt
			opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, fs.Duration(time.Hour)
			opt.CachePollInterval, opt.HandleCaching = 0, 0
			v := New(context.Background(), remote, &opt)
			t.Cleanup(func() { v.Shutdown(); require.NoError(t, v.CleanUp()) })
			for round := range 64 {
				func() {
					old, err := v.OpenFile("target", os.O_CREATE|os.O_RDWR, 0600)
					require.NoError(t, err)
					defer old.Close()
					_, err = old.WriteAt([]byte("old data"), 0)
					require.NoError(t, err)
					before := time.Unix(1700000000, 0)
					require.NoError(t, old.Node().SetModTime(before))
					stop, bad := make(chan struct{}), make(chan time.Time, 1)
					var readers, ready sync.WaitGroup
					var once sync.Once
					finish := func() { once.Do(func() { close(stop); readers.Wait() }) }
					defer finish()
					for range 4 {
						readers.Add(1)
						ready.Add(1)
						go func() {
							defer readers.Done()
							ready.Done()
							for {
								select {
								case <-stop:
									return
								default:
								}
								got := old.Node().ModTime()
								if !got.Equal(before) {
									select {
									case bad <- got:
									default:
									}
									return
								}
								runtime.Gosched()
							}
						}()
					}
					ready.Wait()
					name := "temporary"
					if retirement == "unlink" {
						require.NoError(t, old.Node().Remove())
						name = "target"
					}
					fresh, err := v.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
					require.NoError(t, err)
					defer fresh.Close()
					_, err = fresh.WriteAt([]byte("new data"), 0)
					require.NoError(t, err)
					require.NoError(t, fresh.Node().SetModTime(time.Unix(1800000000, 0)))
					if retirement == "replace" {
						require.NoError(t, v.Rename(name, "target"))
					}
					defer fresh.Node().Remove()
					time.Sleep(time.Millisecond)
					finish()
					select {
					case got := <-bad:
						t.Fatalf("round %d: old inode changed mtime from %v to %v", round, before, got)
					default:
					}
				}()
			}
		})
	}
}

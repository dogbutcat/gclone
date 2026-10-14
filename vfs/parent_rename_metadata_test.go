// Exercise held-file metadata reads while the containing directory is renamed.
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

func TestModTimeDuringParentDirectoryRename(t *testing.T) {
	for _, mode := range []vfscommon.CacheMode{vfscommon.CacheModeOff, vfscommon.CacheModeFull} {
		t.Run(mode.String(), func(t *testing.T) { modTimeDuringParentDirectoryRename(t, mode) })
	}
}

func modTimeDuringParentDirectoryRename(t *testing.T, mode vfscommon.CacheMode) {
	backing := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(backing, "a"), 0700))
	remote, err := fs.NewFs(context.Background(), backing)
	require.NoError(t, err)
	opt := vfscommon.Opt
	opt.CacheMode = mode
	opt.CachePollInterval, opt.HandleCaching, opt.WriteBack = 0, 0, 0
	view := New(context.Background(), remote, &opt)
	t.Cleanup(func() { view.Shutdown(); require.NoError(t, view.CleanUp()) })
	handle, err := view.OpenFile("a/data", os.O_CREATE|os.O_WRONLY, 0600)
	require.NoError(t, err)
	defer func() { require.NoError(t, handle.Close()) }()
	_, err = handle.Write([]byte("pending bytes"))
	require.NoError(t, err)
	file := handle.Node()
	before := time.Unix(1700000000, 0)
	require.NoError(t, file.SetModTime(before))
	stop := make(chan struct{})
	var readers, ready sync.WaitGroup
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
				if got := file.ModTime(); !got.Equal(before) {
					t.Errorf("held-file mtime changed: %v -> %v", before, got)
					return
				}
				runtime.Gosched()
			}
		}()
	}
	var once sync.Once
	finish := func() { once.Do(func() { close(stop) }); readers.Wait() }
	defer finish()
	ready.Wait()
	for range 100 {
		require.NoError(t, view.Rename("a", "b"))
		require.NoError(t, view.Rename("b", "a"))
	}
	finish()
}

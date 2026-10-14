// Regression: a closed-source atomic replace waits for destination metadata I/O.
package vfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscache/writeback"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/require"
)

type renameMetadataGate struct {
	fs.Fs
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (f *renameMetadataGate) NewObject(ctx context.Context, name string) (fs.Object, error) {
	if name == "search_state.json" && f.armed.CompareAndSwap(true, false) {
		close(f.entered)
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.Fs.NewObject(ctx, name)
}

func TestRenameOverRevalidatingReader(t *testing.T) {
	for _, mode := range []string{"idle-reader", "revalidating-reader"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r := fstest.NewRun(t)
			r.WriteObject(ctx, "search_state.json", "{\"v\":0}\n", time.Unix(1, 0))
			remote := &renameMetadataGate{Fs: r.Fremote, entered: make(chan struct{}), release: make(chan struct{})}
			opt := vfscommon.Opt
			opt.CacheMode = vfscommon.CacheModeFull
			v := New(ctx, remote, &opt)
			t.Cleanup(func() {
				for _, entry := range v.cache.Queue()["queue"].([]writeback.QueueInfo) {
					_ = v.cache.QueueSetExpiry(entry.ID, time.Now(), 0)
				}
				cleanupVFS(t, v)
			})
			var once sync.Once
			release := func() { once.Do(func() { close(remote.release) }) }
			defer release()
			root, err := v.Root()
			require.NoError(t, err)
			_, err = v.ReadFile("search_state.json")
			require.NoError(t, err)
			create := func(body string) (string, *RWFileHandle) {
				name := fmt.Sprintf("search_state.json.writing-%d", time.Now().UnixNano())
				h, err := v.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
				require.NoError(t, err)
				_, err = h.Write([]byte(body))
				require.NoError(t, err)
				require.NoError(t, h.Flush())
				require.NoError(t, h.Sync())
				require.NoError(t, h.Close())
				return name, h.(*RWFileHandle)
			}
			first, _ := create("{\"v\":1}\n")
			require.NoError(t, root.Rename(first, "search_state.json", root))
			// A directory refresh observes the older remote destination while
			// its locally replaced successor is still awaiting the default writeback.
			require.NoError(t, root.readDir())
			source, closed := create("{\"v\":2}\n")
			closed.mu.Lock()
			sourceClosed := closed.closed
			closed.mu.Unlock()
			require.True(t, sourceClosed)
			reader, err := v.OpenFile("search_state.json", os.O_RDONLY, 0)
			require.NoError(t, err)
			defer func() { release(); _ = reader.Close() }()
			data := make([]byte, 8)
			if mode == "idle-reader" {
				_, err = reader.ReadAt(data, 0)
				require.NoError(t, err)
			}
			remote.armed.Store(mode == "revalidating-reader")
			readDone := make(chan error, 1)
			go func() { _, err := reader.ReadAt(data, 0); readDone <- err }()
			metadataEntered, readFinished := false, false
			select {
			case <-remote.entered:
				metadataEntered = true
			case err = <-readDone:
				readFinished = true
				require.NoError(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("reader made no progress")
			}
			remote.armed.Store(false)
			target := reader.(*RWFileHandle).file
			targetBusy := !target.muRW.TryLock()
			if !targetBusy {
				target.muRW.Unlock()
			}
			renameDone := make(chan error, 1)
			go func() { renameDone <- root.Rename(source, "search_state.json", root) }()
			var renameErr error
			beforeRelease := false
			select {
			case renameErr = <-renameDone:
				beforeRelease = true
			case <-time.After(500 * time.Millisecond):
			}
			release()
			if !readFinished {
				select {
				case err = <-readDone:
					require.NoError(t, err)
				case <-time.After(3 * time.Second):
					t.Fatal("reader did not resume")
				}
			}
			if !beforeRelease {
				select {
				case renameErr = <-renameDone:
				case <-time.After(3 * time.Second):
					t.Fatal("rename did not finish")
				}
			}
			if mode == "revalidating-reader" {
				require.True(t, metadataEntered)
				require.True(t, targetBusy)
			}
			require.NoError(t, renameErr, "ordinary destination activity must not become EBUSY")
			require.True(t, sourceClosed)
			fresh, err := v.OpenFile("search_state.json", os.O_RDONLY, 0)
			require.NoError(t, err)
			defer fresh.Close()
			newData, err := io.ReadAll(fresh)
			require.NoError(t, err)
			require.Equal(t, "{\"v\":2}\n", string(newData))
			info, err := fresh.Stat()
			require.NoError(t, err)
			mtime, size := info.ModTime(), info.Size()
			require.NoError(t, reader.Close())
			require.Equal(t, mtime, info.ModTime())
			require.Equal(t, size, info.Size())
			current, err := v.Stat("search_state.json")
			require.NoError(t, err)
			require.Same(t, fresh.Node(), current)
		})
	}
}

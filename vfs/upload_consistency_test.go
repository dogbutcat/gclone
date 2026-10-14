// Controlled VFS upload/listing interleaving; no production data or source edits.
package vfs

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/rc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Drive listing objects expose snapshot metadata rather than statting the
// current backing pathname on every Hash/Size call. Model that property here.
type snapshotObject struct {
	fs.Object
	size   int64
	mtime  time.Time
	digest string
}

func (o *snapshotObject) Size() int64                       { return o.size }
func (o *snapshotObject) ModTime(context.Context) time.Time { return o.mtime }
func (o *snapshotObject) Hash(_ context.Context, typ hash.Type) (string, error) {
	if typ == hash.MD5 {
		return o.digest, nil
	}
	return "", hash.ErrUnsupported
}

type listingGate struct {
	fs.Fs
	armed   atomic.Bool
	stale   bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *listingGate) unblock() { f.once.Do(func() { close(f.release) }) }
func (f *listingGate) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	entries, err := f.Fs.List(ctx, dir)
	if err != nil || !f.armed.CompareAndSwap(true, false) {
		return entries, err
	}
	for i, entry := range entries {
		if o, ok := entry.(fs.Object); ok {
			digest, err := o.Hash(ctx, hash.MD5)
			if err != nil {
				return nil, err
			}
			entries[i] = &snapshotObject{Object: o, size: o.Size(), mtime: o.ModTime(ctx), digest: digest}
		}
	}
	close(f.entered)
	<-f.release
	if !f.stale {
		return f.Fs.List(ctx, dir)
	}
	return entries, nil
}

func cacheDataPath(t *testing.T, root string) string {
	t.Helper()
	var found string
	require.NoError(t, filepath.WalkDir(filepath.Join(root, "vfs"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == "queue.sqlite3-wal" {
			found = path
		}
		return nil
	}))
	require.NotEmpty(t, found)
	return found
}

func TestUploadListingShortRead(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-listing-control", true: "pre-upload-listing-applied-late"}[stale], func(t *testing.T) {
			ctx := context.Background()
			previousCache := config.GetCacheDir()
			cacheRoot := t.TempDir()
			require.NoError(t, config.SetCacheDir(cacheRoot))
			t.Cleanup(func() { require.NoError(t, config.SetCacheDir(previousCache)) })
			backing := t.TempDir()
			name := "queue.sqlite3-wal"
			require.NoError(t, os.WriteFile(filepath.Join(backing, name), nil, 0600))
			oldTime := time.Date(2026, 10, 12, 16, 44, 35, 746000000, time.UTC)
			require.NoError(t, os.Chtimes(filepath.Join(backing, name), oldTime, oldTime))
			remote, err := fs.NewFs(ctx, backing)
			require.NoError(t, err)
			gate := &listingGate{Fs: remote, stale: stale, entered: make(chan struct{}), release: make(chan struct{})}
			opt := vfscommon.Opt
			opt.CacheMode = vfscommon.CacheModeFull
			opt.WriteBack = fs.Duration(50 * time.Millisecond)
			opt.HandleCaching, opt.CachePollInterval = 0, 0
			opt.DirCacheTime = fs.Duration(200 * time.Millisecond)
			view := New(ctx, gate, &opt)
			t.Cleanup(func() { gate.unblock(); view.Shutdown(); require.NoError(t, view.CleanUp()) })
			writer, err := view.OpenFile(name, os.O_RDWR, 0600)
			require.NoError(t, err)
			t.Cleanup(func() { _ = writer.Close() })
			payload := bytes.Repeat([]byte("W"), 4284832)
			_, err = writer.WriteAt(payload, 0)
			require.NoError(t, err)
			// Refresh once before upload to create a separate backend-object
			// snapshot on the same File, as ordinary remote listings do.
			root, err := view.Root()
			require.NoError(t, err)
			time.Sleep(225 * time.Millisecond)
			_, err = root.ReadDirAll()
			require.NoError(t, err)
			reader, err := view.OpenFile(name, os.O_RDWR, 0600)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reader.Close() })
			node := reader.Node().(*File)
			require.Same(t, writer.Node(), reader.Node())
			beforeObject := node.DirEntry()
			t.Logf("pre-upload File=%p object=%p size=%d", node, beforeObject, beforeObject.Size())
			time.Sleep(225 * time.Millisecond)
			gate.armed.Store(true)
			listed := make(chan error, 1)
			go func() {
				_, err := root.ReadDirAll()
				listed <- err
			}()
			select {
			case <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("listing never captured old metadata")
			}
			require.NoError(t, writer.Close())
			// The actual uploader installs the fresh object on this SAME File,
			// then its directory publication blocks behind the older listing.
			require.Eventually(t, func() bool { return node.DirEntry() != beforeObject && node.DirEntry().Size() == int64(len(payload)) }, 5*time.Second, time.Millisecond)
			t.Logf("upload callback File=%p fresh object=%p size=%d", node, node.DirEntry(), node.DirEntry().Size())
			gate.unblock()
			select {
			case err := <-listed:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("listing did not return")
			}
			require.Eventually(t, func() bool {
				s := view.Stats()["diskCache"].(rc.Params)
				return s["uploadsQueued"] == 0 && s["uploadsInProgress"] == 0
			}, 5*time.Second, time.Millisecond)
			dataPath := cacheDataPath(t, cacheRoot)
			before, err := os.Stat(dataPath)
			require.NoError(t, err)
			t.Logf("after completed upload: same File=%p object size=%d cache size=%d", node, node.DirEntry().Size(), before.Size())
			buf := make([]byte, 8192)
			n, readErr := reader.ReadAt(buf, 3227648)
			after, err := os.Stat(dataPath)
			require.NoError(t, err)
			remoteBytes, err := os.ReadFile(filepath.Join(backing, name))
			require.NoError(t, err)
			t.Logf("RESULT n=%d err=%v cache_before=%d cache_after=%d backing_size=%d backing_md5=%x", n, readErr, before.Size(), after.Size(), len(remoteBytes), md5.Sum(remoteBytes))
			require.Equal(t, payload, remoteBytes, "remote backing is still intact")
			assert.Equal(t, len(buf), n, "a completed upload must not be invalidated by an earlier listing")
			assert.NoError(t, readErr)
			if n > 0 {
				assert.Equal(t, payload[3227648:3227648+n], buf[:n])
			}
			if readErr != nil && readErr != io.EOF {
				t.Log(fmt.Sprintf("unexpected error category: %v", readErr))
			}
			require.NoError(t, reader.Close())
		})
	}
}

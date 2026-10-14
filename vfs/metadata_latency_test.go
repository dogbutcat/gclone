// Regression coverage for slow metadata during normal asynchronous publication.
package vfs

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/require"
)

type metadataLatencyFS struct {
	fs.Fs
	features fs.Features
	armed    atomic.Bool
	entered  chan struct{}
	release  chan struct{}
}

func (f *metadataLatencyFS) Features() *fs.Features { return &f.features }

func (f *metadataLatencyFS) NewObject(ctx context.Context, name string) (fs.Object, error) {
	if name == "wal" && f.armed.CompareAndSwap(true, false) {
		close(f.entered)
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.Fs.NewObject(ctx, name)
}

func TestAsyncMetadataLookupDoesNotBlockDirectory(t *testing.T) {
	for _, flags := range []int{os.O_RDONLY, os.O_RDWR} {
		t.Run(map[int]string{os.O_RDONLY: "read-only", os.O_RDWR: "active-writer"}[flags], func(t *testing.T) {
			ctx := context.Background()
			r := fstest.NewRun(t)
			r.WriteObject(ctx, "wal", "old", time.Unix(1, 0))
			require.NoError(t, r.Fremote.Mkdir(ctx, "unrelated"))
			remote := &metadataLatencyFS{Fs: r.Fremote, features: *r.Fremote.Features(), entered: make(chan struct{}), release: make(chan struct{})}
			remote.features.Copy, remote.features.Move = nil, nil
			opt := vfscommon.Opt
			opt.CacheMode = vfscommon.CacheModeFull
			opt.HandleCaching, opt.CachePollInterval, opt.PollInterval = 0, 0, 0
			opt.WriteBack, opt.DirCacheTime = fs.Duration(20*time.Millisecond), fs.Duration(time.Hour)
			v := New(ctx, remote, &opt)
			t.Cleanup(func() { cleanupVFS(t, v) })
			root, err := v.Root()
			require.NoError(t, err)
			node, err := root.Stat("wal")
			require.NoError(t, err)
			file := node.(*File)
			writer, err := file.Open(os.O_WRONLY)
			require.NoError(t, err)
			_, err = writer.Write([]byte("uploaded"))
			require.NoError(t, err)
			reader, err := file.Open(flags)
			require.NoError(t, err)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(remote.release) }) }
			defer func() { release(); _ = reader.Close() }()

			// Real asynchronous writeback publishes File.o, then waits on Dir.mu.
			// The closing writer has already released File.muRW and deregistered.
			_, revision := file.objectSnapshot()
			root.mu.Lock()
			locked := true
			defer func() {
				if locked {
					root.mu.Unlock()
				}
			}()
			require.NoError(t, writer.Close())
			require.Eventually(t, func() bool {
				_, current := file.objectSnapshot()
				return current != revision
			}, 5*time.Second, time.Millisecond)
			remote.armed.Store(true)
			readDone := make(chan error, 1)
			data := make([]byte, 8)
			go func() { _, err := reader.ReadAt(data, 0); readDone <- err }()
			select {
			case <-remote.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("deferred handle did not reach publication revalidation")
			}
			root.mu.Unlock()
			locked = false
			forgotten := make(chan struct{})
			go func() { root.ForgetAll(); close(forgotten) }()
			forgetReady := false
			select {
			case <-forgotten:
				forgetReady = true
			case <-time.After(time.Second):
			}
			lookups := make(chan error, 16)
			for range 16 {
				go func() { _, err := root.Stat("unrelated"); lookups <- err }()
			}
			completed := 0
			timer := time.NewTimer(time.Second)
		loop:
			for completed < 16 {
				select {
				case err := <-lookups:
					require.NoError(t, err)
					completed++
				case <-timer.C:
					break loop
				}
			}
			timer.Stop()
			release()
			require.NoError(t, <-readDone)
			require.Equal(t, "uploaded", string(data))
			<-forgotten
			for remaining := completed; remaining < 16; remaining++ {
				require.NoError(t, <-lookups)
			}
			require.True(t, forgetReady, "virtual cleanup waited for remote metadata")
			require.Equal(t, 16, completed, "unrelated directory lookups waited for remote metadata")
		})
	}
}

func TestSyncPublicationSerializesDeferredOpen(t *testing.T) {
	ctx := context.Background()
	r := fstest.NewRun(t)
	r.WriteObject(ctx, "wal", "old", time.Unix(1, 0))
	remote := &metadataLatencyFS{Fs: r.Fremote, features: *r.Fremote.Features(), entered: make(chan struct{}), release: make(chan struct{})}
	remote.features.Copy, remote.features.Move = nil, nil
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.WriteBack, opt.HandleCaching, opt.CachePollInterval, opt.PollInterval = 0, 0, 0, 0
	v := New(ctx, remote, &opt)
	t.Cleanup(func() { cleanupVFS(t, v) })
	root, err := v.Root()
	require.NoError(t, err)
	node, err := root.Stat("wal")
	require.NoError(t, err)
	file := node.(*File)
	writer, err := file.Open(os.O_WRONLY)
	require.NoError(t, err)
	_, err = writer.Write([]byte("uploaded"))
	require.NoError(t, err)
	reader, err := file.Open(os.O_RDONLY)
	require.NoError(t, err)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(remote.release) }) }
	defer func() { release(); _ = reader.Close() }()
	_, revision := file.objectSnapshot()
	root.mu.Lock()
	locked := true
	defer func() {
		if locked {
			root.mu.Unlock()
		}
	}()
	closeDone := make(chan error, 1)
	go func() { closeDone <- writer.Close() }()
	require.Eventually(t, func() bool {
		_, current := file.objectSnapshot()
		return current != revision
	}, 5*time.Second, time.Millisecond)
	remote.armed.Store(true)
	readDone := make(chan error, 1)
	data := make([]byte, 8)
	go func() { _, err := reader.ReadAt(data, 0); readDone <- err }()
	select {
	case <-remote.entered:
		t.Error("synchronous close must serialize the same File's deferred open")
	case <-time.After(20 * time.Millisecond):
	}
	root.mu.Unlock()
	locked = false
	release()
	require.NoError(t, <-closeDone)
	require.NoError(t, <-readDone)
	require.Equal(t, "uploaded", string(data))
}

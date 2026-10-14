// Regress sparse extension publication across concurrent stat and cancellation.
package vfscache

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

func TestSparseWritePublishesBeforeCancellation(t *testing.T) {
	opt := vfscommon.Opt
	opt.CachePollInterval, opt.HandleCaching = 0, 0
	opt.WriteBack = fs.Duration(time.Hour)
	_, c := newTestCacheOpt(t, opt)
	item := c.Item("index-shm")
	require.NoError(t, item.Open(nil))
	_, err := item.WriteAt([]byte{0}, 0)
	require.NoError(t, err)
	require.NoError(t, item.Close(nil)) // Leaves an ordinary queued writeback.
	require.NoError(t, item.Open(nil))
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); _ = item.Close(nil); c.writeback.Remove(item.writeBackID) })
	c.writeback.Add(item.writeBackID, item.name, 1, false, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	})
	require.NoError(t, c.writeback.SetExpiry(item.writeBackID, time.Now(), 0))
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("writeback did not reach barrier")
	}
	done := make(chan error, 1)
	go func() { _, err := item.WriteAt([]byte{'Z'}, 32767); done <- err }()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("extension did not cancel the pending writeback")
	}
	size, err := item.GetSize() // Real stat while the write awaits cancellation.
	require.NoError(t, err)
	require.Equal(t, int64(32768), size)
	unblock()
	require.NoError(t, <-done)
	got := make([]byte, 32768)
	n, err := item.ReadAt(got, 0)
	require.NoError(t, err, "the local zero extent must not be downloaded")
	require.Equal(t, len(got), n)
	want := make([]byte, len(got))
	want[len(want)-1] = 'Z'
	require.True(t, bytes.Equal(want, got))
}

func TestEmptyWriteDoesNotPublishSparseExtent(t *testing.T) {
	_, c := newItemTestCache(t)
	item := c.Item("empty")
	require.NoError(t, item.Open(nil))
	defer item.Close(nil)
	before := item.info
	n, err := item.WriteAt(nil, 32767)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, before.Size, item.info.Size)
	require.Equal(t, before.Rs, item.info.Rs)
	require.True(t, before.ModTime.Equal(item.info.ModTime))
}

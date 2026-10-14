// Keep application modification times independent of cache reads and close.
package vfscache

import (
	"context"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscache/writeback"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

func TestDirtyModTimeSurvivesCacheFillAndClose(t *testing.T) {
	opt := vfscommon.Opt
	opt.CachePollInterval, opt.HandleCaching = 0, 0
	opt.WriteBack = fs.Duration(time.Hour)
	r, c := newTestCacheOpt(t, opt)
	t.Cleanup(func() {
		for _, entry := range c.Queue()["queue"].([]writeback.QueueInfo) {
			_ = c.QueueSetExpiry(entry.ID, time.Now(), 0)
		}
	})
	r.WriteObject(context.Background(), "metadata", "abcdefgh", time.Unix(100, 0))
	o, err := r.Fremote.NewObject(context.Background(), "metadata")
	require.NoError(t, err)
	item := c.Item("metadata")
	require.NoError(t, item.Open(o))
	_, err = item.WriteAt([]byte("WXYZ"), 4)
	require.NoError(t, err)
	before, err := item.GetModTime()
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)
	buf := make([]byte, 4)
	_, err = item.ReadAt(buf, 0) // Filling untouched bytes must not change mtime.
	require.NoError(t, err)
	require.Equal(t, "abcd", string(buf))
	afterRead, err := item.GetModTime()
	require.NoError(t, err)
	require.True(t, before.Equal(afterRead), "cache download changed mtime: %v -> %v", before, afterRead)
	require.NoError(t, item.Close(nil))
	afterClose, err := item.GetModTime()
	require.NoError(t, err)
	require.True(t, before.Equal(afterClose), "close changed mtime: %v -> %v", before, afterClose)
}

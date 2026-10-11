// gclone-local regression: isolate descriptors retained across rename-overwrite.
package vfs

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscache/writeback"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenameOverReadOnlyLifetime(t *testing.T) {
	for _, source := range []string{"dirty-cache", "clean-cache", "uncached"} {
		t.Run(source, func(t *testing.T) {
			opt := vfscommon.Opt
			opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, fs.Duration(time.Hour)
			opt.HandleCaching, opt.CachePollInterval = 0, 0
			r, v := newTestVFSOpt(t, &opt)
			// Bound cleanup even if an assertion interrupts a long-delay fixture.
			t.Cleanup(func() {
				for _, entry := range v.cache.Queue()["queue"].([]writeback.QueueInfo) {
					_ = v.cache.QueueSetExpiry(entry.ID, time.Now(), 0)
				}
			})
			r.WriteObject(context.Background(), "target", "old data", t1)
			if source != "dirty-cache" {
				r.WriteObject(context.Background(), "temporary", "new data", t2)
			}
			reader, err := v.OpenFile("target", os.O_RDONLY, 0)
			require.NoError(t, err)
			old := reader.(*RWFileHandle)
			t.Cleanup(func() { _ = old.Close() })
			buf := make([]byte, 8)
			_, err = old.ReadAt(buf, 0)
			require.NoError(t, err)
			oldObject := old.file.getObject()
			var pending Handle
			if source == "dirty-cache" {
				pending, err = v.OpenFile("temporary", os.O_CREATE|os.O_RDWR, 0600)
				require.NoError(t, err)
				_, err = pending.Write([]byte("new data"))
				require.NoError(t, err)
				require.NoError(t, pending.Sync())
				require.NoError(t, pending.Close())
			} else {
				if source == "clean-cache" {
					_, err = v.ReadFile("temporary")
					require.NoError(t, err)
				}
			}
			root, err := v.Root()
			require.NoError(t, err)
			require.NoError(t, root.Rename("temporary", "target", root))
			fresh, err := v.OpenFile("target", os.O_RDONLY, 0)
			require.NoError(t, err)
			t.Cleanup(func() { _ = fresh.Close() })
			_, err = fresh.ReadAt(buf, 0)
			require.NoError(t, err)
			assert.Equal(t, "new data", string(buf))
			// Stat returns a live File; snapshot scalar values before old close.
			info, err := fresh.Stat()
			require.NoError(t, err)
			beforeTime, beforeSize := info.ModTime(), info.Size()
			require.NoError(t, old.Close())
			assert.Equal(t, beforeTime, info.ModTime())
			assert.Equal(t, beforeSize, info.Size())
			old.file.setObject(oldObject)
			node, err := v.Stat("target")
			require.NoError(t, err)
			assert.Same(t, fresh.Node(), node, "old callbacks must not republish the displaced target")
			require.NoError(t, fresh.Close())
		})
	}
}

func TestRenameOverDeferredRead(t *testing.T) {
	opt := vfscommon.Opt
	opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, 0
	opt.HandleCaching, opt.CachePollInterval = 0, 0
	r, v := newTestVFSOpt(t, &opt)
	r.WriteObject(context.Background(), "source", "new data", t2)
	r.WriteObject(context.Background(), "target", "old data", t1)
	old, err := v.OpenFile("target", os.O_RDONLY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = old.Close() })
	require.NoError(t, v.Rename("source", "target"))
	_, err = old.Read(make([]byte, 8))
	assert.ErrorIs(t, err, ENOENT, "deferred old reader must not attach to the replacement")
	data, err := v.ReadFile("target")
	require.NoError(t, err)
	assert.Equal(t, "new data", string(data))
	// Same-path rename is a no-op and must not retire the live generation.
	require.NoError(t, v.Rename("target", "target"))
	data, err = v.ReadFile("target")
	require.NoError(t, err)
	assert.Equal(t, "new data", string(data))
}

func TestRenameOverOpenWriter(t *testing.T) {
	opt := vfscommon.Opt
	opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, writeBackDelay
	opt.HandleCaching, opt.CachePollInterval = 0, 0
	r, v := newTestVFSOpt(t, &opt)
	r.WriteObject(context.Background(), "source", "new data", t2)
	r.WriteObject(context.Background(), "target", "old data", t1)
	old, err := v.OpenFile("target", os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = old.Close() })
	_, err = old.WriteAt([]byte("old edit"), 0)
	require.NoError(t, err)
	require.NoError(t, v.Rename("source", "target"))
	_, err = old.WriteAt([]byte("late edit"), 0)
	require.NoError(t, err)
	require.NoError(t, old.Sync())
	require.NoError(t, old.Close())
	v.WaitForWriters(waitForWritersDelay)
	data, err := v.ReadFile("target")
	require.NoError(t, err)
	assert.Equal(t, "new data", string(data))
	assert.Equal(t, 0, v.cache.Stats()["uploadsQueued"])
}

func TestRenameAbortKeepsSourceIdentity(t *testing.T) {
	opt := vfscommon.Opt
	opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, fs.Duration(time.Hour)
	opt.HandleCaching, opt.CachePollInterval = 0, 0
	r, v := newTestVFSOpt(t, &opt)
	t.Cleanup(func() {
		for _, entry := range v.cache.Queue()["queue"].([]writeback.QueueInfo) {
			_ = v.cache.QueueSetExpiry(entry.ID, time.Now(), 0)
		}
	})
	r.WriteObject(context.Background(), "source", "source bytes", t2)
	target, err := v.OpenFile("target", os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	_, err = target.Write([]byte("target bytes"))
	require.NoError(t, err)
	require.NoError(t, target.Close())
	source, err := v.Stat("source")
	require.NoError(t, err)
	features := r.Fremote.Features()
	move := features.Move
	moveErr := errors.New("rename denied")
	features.Move = func(context.Context, fs.Object, string) (fs.Object, error) { return nil, moveErr }
	t.Cleanup(func() { features.Move = move })
	require.ErrorIs(t, v.Rename("source", "target"), moveErr)
	assert.Equal(t, "source", source.Path())
	current, err := v.Stat("target")
	require.NoError(t, err)
	assert.Same(t, target.Node(), current)
	data, err := v.ReadFile("target")
	require.NoError(t, err)
	assert.Equal(t, "target bytes", string(data))
}

func TestRenameDeferredMinimalRetiresTarget(t *testing.T) {
	opt := vfscommon.Opt
	opt.CacheMode, opt.WriteBack = vfscommon.CacheModeMinimal, 0
	opt.HandleCaching, opt.CachePollInterval = 0, 0
	r, v := newTestVFSOpt(t, &opt)
	r.WriteObject(context.Background(), "target", "old data", t1)
	// O_RDWR forces a cached target while the write-only source is uncached.
	old, err := v.OpenFile("target", os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = old.Close() })
	_, err = old.ReadAt(make([]byte, 8), 0)
	require.NoError(t, err)
	source, err := v.OpenFile("source", os.O_WRONLY|os.O_CREATE, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	_, err = source.Write([]byte("new data"))
	require.NoError(t, err)
	require.NoError(t, v.Rename("source", "target"))
	assert.True(t, old.Node().(*File).unlinked)
	_, err = old.WriteAt([]byte("old edit"), 0)
	require.NoError(t, err)
	require.NoError(t, old.Close())
	require.NoError(t, source.Close())
	data, err := v.ReadFile("target")
	require.NoError(t, err)
	assert.Equal(t, "new data", string(data))
}

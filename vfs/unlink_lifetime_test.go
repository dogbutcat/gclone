// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

// Regression coverage for unlink versus delayed writes and namespace publication.
package vfs

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnlinkOpenWriterLifetime(t *testing.T) {
	for _, delay := range []fs.Duration{0, writeBackDelay} {
		for _, recreate := range []bool{false, true} {
			t.Run(fmt.Sprintf("delay=%v/recreate=%v", delay, recreate), func(t *testing.T) {
				opt := vfscommon.Opt
				opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, delay
				opt.HandleCaching, opt.CachePollInterval = 0, 0
				r, v := newTestVFSOpt(t, &opt)
				r.WriteObject(context.Background(), "db-shm", "0123456789abcdef", t1)
				h, err := v.OpenFile("db-shm", os.O_RDWR, 0600)
				require.NoError(t, err)
				old := h.(*RWFileHandle)
				t.Cleanup(func() { _ = old.Close() })
				buf := make([]byte, 16)
				_, err = old.ReadAt(buf, 0)
				require.NoError(t, err)
				require.NoError(t, old.file.Remove())
				_, err = old.ReadAt(buf, 0)
				require.NoError(t, err)
				assert.Equal(t, "0123456789abcdef", string(buf), "unlink must preserve cached ranges")

				var replacement Handle
				if recreate {
					replacement, err = v.OpenFile("db-shm", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
					require.NoError(t, err)
					t.Cleanup(func() { _ = replacement.Close() })
					_, err = replacement.Write([]byte("new generation"))
					require.NoError(t, err)
				}
				// Late kernel writes target the old inode, never the reused pathname.
				_, err = old.WriteAt([]byte("old handle bytes"), 0)
				require.NoError(t, err)
				require.NoError(t, old.Truncate(16))
				require.NoError(t, old.Sync())
				assert.Equal(t, int64(16), old.file.Size(), "old inode must not use replacement cache metadata")
				require.NoError(t, old.Close())
				require.Eventually(t, func() bool {
					stats := v.cache.Stats()
					return stats["uploadsInProgress"] == 0 && stats["uploadsQueued"] == 0
				}, 5*time.Second, time.Millisecond)
				if recreate {
					node, err := v.Stat("db-shm")
					require.NoError(t, err)
					assert.Same(t, replacement.Node(), node)
					require.NoError(t, replacement.Close())
					v.WaitForWriters(waitForWritersDelay)
					contents, err := v.ReadFile("db-shm")
					require.NoError(t, err)
					assert.Equal(t, "new generation", string(contents))
				} else {
					_, err := v.Stat("db-shm")
					assert.ErrorIs(t, err, ENOENT, "old close must not resurrect the pathname")
					_, err = r.Fremote.NewObject(context.Background(), "db-shm")
					assert.ErrorIs(t, err, fs.ErrorObjectNotFound)
					assert.False(t, v.cache.Exists("db-shm"))
				}
			})
		}
	}
}

func TestUnlinkRejectsStalePublication(t *testing.T) {
	_, v, old := rwHandleCreateFlags(t, true, "db-shm", os.O_RDWR)
	t.Cleanup(func() { _ = old.Close() })
	o := old.file.getObject()
	require.NoError(t, old.file.Remove())
	replacement, err := v.OpenFile("db-shm", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = replacement.Close() })
	// Simulate a completion retaining the old object after unlink returns.
	old.file.setObject(o)
	node, err := v.Stat("db-shm")
	require.NoError(t, err)
	assert.Same(t, replacement.Node(), node)
	// A deferred old open must not reconnect to the new generation's cache.
	_, err = old.WriteAt([]byte("stale"), 0)
	assert.Error(t, err)
	require.NoError(t, replacement.Close())
}

type unlinkDeleteErrorObject struct {
	fs.Object
}

func (o *unlinkDeleteErrorObject) Remove(context.Context) error { return os.ErrPermission }

func TestUnlinkFailureKeepsFileLifetime(t *testing.T) {
	_, v, old := rwHandleCreateFlags(t, true, "db-shm", os.O_RDWR)
	t.Cleanup(func() { _ = old.Close() })
	old.file.setObject(&unlinkDeleteErrorObject{Object: old.file.getObject()})
	buf := make([]byte, 16)
	_, err := old.ReadAt(buf, 0)
	require.NoError(t, err)
	require.ErrorIs(t, old.file.Remove(), os.ErrPermission)
	node, err := v.Stat("db-shm")
	require.NoError(t, err)
	assert.Same(t, old.file, node)
	_, err = old.WriteAt([]byte("write after fail"), 0)
	require.NoError(t, err)
	require.NoError(t, old.Close())
	v.WaitForWriters(waitForWritersDelay)
	contents, err := v.ReadFile("db-shm")
	require.NoError(t, err)
	assert.Equal(t, "write after fail", string(contents))
}

func TestUnlinkExcludesDestinationRename(t *testing.T) {
	opt := vfscommon.Opt
	opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, 0
	opt.HandleCaching, opt.CachePollInterval = 0, 0
	r, v := newTestVFSOpt(t, &opt)
	r.WriteObject(context.Background(), "source", "source bytes", t1)
	r.WriteObject(context.Background(), "target", "target bytes", t1)
	source, err := v.Stat("source")
	require.NoError(t, err)
	root, err := v.Root()
	require.NoError(t, err)
	done := make(chan error, 1)
	removed, err := v.cache.Unlink("target", func(fs.Object) error {
		go func() { done <- root.Rename("source", "target", root) }()
		select {
		case err := <-done:
			t.Errorf("rename returned while unlink owned the pathname: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		assert.Equal(t, "source", source.Path(), "waiting must precede File identity change")
		return os.ErrPermission
	})
	require.ErrorIs(t, err, os.ErrPermission)
	assert.False(t, removed)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("rename failed to resume after unlink rollback")
	}
	data, err := v.ReadFile("target")
	require.NoError(t, err)
	assert.Equal(t, "source bytes", string(data))
}

func TestRenameReservationExcludesUnlink(t *testing.T) {
	opt := vfscommon.Opt
	opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, 0
	opt.HandleCaching, opt.CachePollInterval = 0, 0
	r, v := newTestVFSOpt(t, &opt)
	r.WriteObject(context.Background(), "source", "source bytes", t1)
	r.WriteObject(context.Background(), "target", "target bytes", t1)
	root, err := v.Root()
	require.NoError(t, err)
	mutation, err := v.cache.BeginRename("source", "target")
	require.NoError(t, err)
	assert.ErrorIs(t, root.RemoveName("target"), syscall.EBUSY)
	mutation.Close()
	mutation.Close() // release is idempotent
	require.NoError(t, root.Rename("source", "target", root))
	data, err := v.ReadFile("target")
	require.NoError(t, err)
	assert.Equal(t, "source bytes", string(data))
}

type unlinkBlockedDeleteObject struct {
	fs.Object
	entered, release chan struct{}
}

func (o *unlinkBlockedDeleteObject) Remove(context.Context) error {
	close(o.entered)
	<-o.release
	return os.ErrPermission
}

func TestUnlinkDoesNotAcknowledgeLostModTime(t *testing.T) {
	_, _, old := rwHandleCreateFlags(t, true, "db-shm", os.O_RDWR)
	t.Cleanup(func() { _ = old.Close() })
	o := &unlinkBlockedDeleteObject{Object: old.file.getObject(), entered: make(chan struct{}), release: make(chan struct{})}
	old.file.setObject(o)
	done := make(chan error, 1)
	go func() { done <- old.file.Remove() }()
	select {
	case <-o.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not start")
	}
	assert.ErrorIs(t, old.file.SetModTime(t3), syscall.EBUSY)
	close(o.release)
	select {
	case err := <-done:
		require.ErrorIs(t, err, os.ErrPermission)
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not finish")
	}
	assert.True(t, t1.Equal(old.file.getObject().ModTime(context.Background())))
}

// Regression coverage for unlocked metadata requests and stale-result rejection.
package vfscache

import (
	"context"
	"errors"
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

type metadataGate struct {
	fs.Fs
	object  fs.Object
	err     error
	entered chan string
	release chan struct{}
	once    sync.Once
	armed   atomic.Bool
}

func (f *metadataGate) NewObject(ctx context.Context, name string) (fs.Object, error) {
	if !f.armed.CompareAndSwap(true, false) {
		return f.Fs.NewObject(ctx, name)
	}
	f.once.Do(func() { f.entered <- name })
	select {
	case <-f.release:
		return f.object, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func installMetadataGate(t *testing.T, c *Cache, object fs.Object, err error) (*metadataGate, func()) {
	t.Helper()
	gate := c.fremote.(*metadataGate)
	gate.object, gate.err = object, err
	gate.armed.Store(true)
	var once sync.Once
	release := func() { once.Do(func() { close(gate.release) }) }
	t.Cleanup(release)
	return gate, release
}

func metadataCacheFixture(t *testing.T) (*fstest.Run, *Cache, *Item) {
	t.Helper()
	r := fstest.NewRun(t)
	remote := &metadataGate{Fs: r.Fremote, entered: make(chan string, 1), release: make(chan struct{})}
	opt := vfscommon.Opt
	opt.WriteBack, opt.HandleCaching, opt.CachePollInterval = 0, 0, 0
	ctx, cancel := context.WithCancel(context.Background())
	c, err := New(ctx, remote, &opt, nil)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); require.NoError(t, c.CleanUp()) })
	r.WriteObject(ctx, "wal", "", time.Unix(1, 0))
	o, err := r.Fremote.NewObject(ctx, "wal")
	require.NoError(t, err)
	item := c.Item("wal")
	require.NoError(t, item.Open(o))
	_, err = item.WriteAt([]byte("uploaded bytes"), 0)
	require.NoError(t, err)
	require.NoError(t, item.Close(nil))
	return r, c, item
}

func waitMetadata(t *testing.T, gate *metadataGate) {
	t.Helper()
	select {
	case name := <-gate.entered:
		require.Equal(t, "wal", name)
	case <-time.After(5 * time.Second):
		t.Fatal("metadata lookup did not reach the barrier")
	}
}

func TestMetadataOpenRejectsSupersededReplies(t *testing.T) {
	for _, reply := range []string{"success", "absent", "error"} {
		for _, change := range []string{"write", "rename", "unlink", "publication", "new-absence"} {
			t.Run(reply+"/"+change, func(t *testing.T) {
				r, c, item := metadataCacheFixture(t)
				current := item.o
				require.NoError(t, item.Open(current))
				before, err := os.ReadFile(c.toOSPath("wal"))
				require.NoError(t, err)
				var lookupErr error
				if reply == "absent" {
					lookupErr = fs.ErrorObjectNotFound
				} else if reply == "error" {
					lookupErr = os.ErrPermission
				}
				gate, release := installMetadataGate(t, c, current, lookupErr)
				item.mu.Lock()
				item.mustRevalidate = true
				if change == "publication" {
					item.publishing, item.info.Dirty = true, true
				}
				item.mu.Unlock()
				done := make(chan error, 1)
				go func() { _, err := item.openWithObject(current, false, false); done <- err }()
				waitMetadata(t, gate)
				path := "wal"
				switch change {
				case "write":
					_, err = item.WriteAt([]byte("LOCAL"), 0)
					require.NoError(t, err)
				case "rename":
					moved, err := r.Fremote.Features().Move(context.Background(), current, "moved")
					require.NoError(t, err)
					require.NoError(t, c.Rename("wal", "moved", moved))
					path = "moved"
				case "unlink":
					removed, err := c.Unlink("wal", func(o fs.Object) error { return o.Remove(context.Background()) })
					require.NoError(t, err)
					require.True(t, removed)
				case "publication":
					// Callback completion changes these predicates without changing
					// objectEpoch. It must still supersede the request's assumptions.
					item.mu.Lock()
					item.publishing, item.info.Dirty = false, false
					item.mu.Unlock()
				case "new-absence":
					item.mu.Lock()
					item._observeAbsence()
					item.mu.Unlock()
				}
				release()
				require.ErrorIs(t, <-done, errObjectViewChanged, "old reply must not commit or replace the newer error/state")
				item.mu.Lock()
				opens, pending := item.opens, item.pendingLookups
				item.mu.Unlock()
				require.Equal(t, 1, opens, "failed open must not acquire a handle")
				require.Zero(t, pending)
				if change != "unlink" {
					after, err := os.ReadFile(c.toOSPath(path))
					require.NoError(t, err)
					if change == "write" {
						copy(before, "LOCAL")
						require.True(t, item.IsDirty())
					}
					require.Equal(t, before, after, "delayed reply must not discard cache bytes")
				} else {
					_, err = os.Stat(c.toOSPath("wal"))
					require.ErrorIs(t, err, os.ErrNotExist)
				}
				require.NoError(t, item.Close(nil))
			})
		}
	}
}

func TestMetadataWritebackRejectsConcurrentWrite(t *testing.T) {
	r, c, item := metadataCacheFixture(t)
	current := item.o
	require.NoError(t, item.Open(current))
	_, err := item.WriteAt([]byte("first"), 0)
	require.NoError(t, err)
	item.mu.Lock()
	item.mustRevalidate = true
	item.mu.Unlock()
	gate, release := installMetadataGate(t, c, nil, fs.ErrorObjectNotFound)
	done := make(chan error, 1)
	go func() { done <- item.store(context.Background(), nil) }()
	waitMetadata(t, gate)
	_, err = item.WriteAt([]byte("LOCAL"), 0)
	require.NoError(t, err)
	release()
	require.ErrorIs(t, <-done, errObjectViewChanged)
	require.True(t, item.IsDirty())
	checkObject(t, r, "wal", "uploaded bytes")
	require.NoError(t, item.Close(nil))
	checkObject(t, r, "wal", "LOCALded bytes")
}

func TestMetadataLookupDoesNotBlockCacheMaintenance(t *testing.T) {
	_, c, item := metadataCacheFixture(t)
	current := item.o
	gate, release := installMetadataGate(t, c, current, nil)
	item.mu.Lock()
	item.mustRevalidate = true
	item.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := item.OpenWithObject(current, false); done <- err }()
	waitMetadata(t, gate)
	maintenance := make(chan error, 1)
	go func() {
		if !c.InUse("wal") {
			maintenance <- errors.New("pending lookup lost its cache pin")
			return
		}
		c.mu.Lock() // same outer lock as purgeClean/retryFailedResets
		result, freed, err := item.Reset()
		c.mu.Unlock()
		if err != nil || result != SkippedPendingAccess || freed != 0 {
			maintenance <- errors.New("cleaner changed an item with an in-flight lookup")
			return
		}
		if removed, _ := item.RemoveNotInUse(0, false); removed {
			maintenance <- errors.New("cleaner evicted an in-flight lookup")
			return
		}
		_ = c.DirtyItem("wal")
		_ = c.Item("unrelated")
		maintenance <- nil
	}()
	select {
	case err := <-maintenance:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("cache operations waited for the remote lookup")
	}
	release()
	require.NoError(t, <-done)
	require.NoError(t, item.Close(nil))
}

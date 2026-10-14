// Preserve sparse dirty data and local creation ownership after remote deletion.
package vfscache

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSparseDirtyAbsenceMustNotUploadHoles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := fstest.NewRun(t)
	opt := vfscommon.Opt
	opt.WriteBack, opt.HandleCaching, opt.CachePollInterval = 0, 0, 0
	opt.FastFingerprint = true
	c, err := New(ctx, r.Fremote, &opt, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		require.NoError(t, c.CleanUp())
	})
	r.WriteObject(ctx, "sparse", "abcdefghij", time.Unix(1, 0))
	original, err := r.Fremote.NewObject(ctx, "sparse")
	require.NoError(t, err)
	snapshot := cacheSnapshot(t, original)
	item := c.Item("sparse")
	_, err = item.OpenWithObject(snapshot, false)
	require.NoError(t, err)
	require.Zero(t, item.info.Rs.Size(), "existing remote bytes have not been downloaded")
	require.NoError(t, original.Remove(ctx))
	_, err = item.OpenWithObject(nil, false)
	require.ErrorIs(t, err, os.ErrNotExist, "first confirm absence while original handle remains open")
	_, err = item.WriteAt([]byte("X"), 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), item.info.Rs.Size())
	_, err = item.OpenWithObject(snapshot, false)
	require.NoError(t, err, "pending local bytes may remain accessible")
	t.Logf("before close: objectNil=%v size=%d ranges=%v", item.o == nil, item.info.Size, item.info.Rs)
	require.NoError(t, item.Close(nil))
	closeErr := item.Close(nil)
	t.Logf("last close: %v", closeErr)
	remote, lookupErr := r.Fremote.NewObject(ctx, "sparse")
	if lookupErr == nil {
		stream, openErr := remote.Open(ctx)
		require.NoError(t, openErr)
		data, readErr := io.ReadAll(stream)
		require.NoError(t, readErr)
		require.NoError(t, stream.Close())
		t.Logf("uploaded bytes: %q; dirty=%v ranges=%v", data, item.info.Dirty, item.info.Rs)
	}
	assert.ErrorIs(t, lookupErr, fs.ErrorObjectNotFound, "unavailable original ranges must not be silently published as zeros")
	assert.True(t, item.info.Dirty, "incomplete local changes must stay pending")
	require.Error(t, closeErr)
	// Reopening after the failed close must retain the modified byte, and
	// an explicit write can supply the otherwise unavailable original ranges.
	_, err = item.OpenWithObject(snapshot, false)
	require.NoError(t, err)
	_, err = item.WriteAt([]byte("bcdefghij"), 1)
	require.NoError(t, err)
	require.NoError(t, item.Close(nil))
	checkObject(t, r, "sparse", "Xbcdefghij")
}

// A committed local creation must not reacquire an already deleted ID.
type createdOwnerSnapshotID struct {
	fs.Object
	id string
}

func (o *createdOwnerSnapshotID) ID() string { return o.id }

func TestCreatedOwnerRejectsOldSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := fstest.NewRun(t)
	remote := &publicationIDFS{Fs: r.Fremote, features: *r.Fremote.Features()}
	remote.features.Copy, remote.features.Move = nil, nil
	remote.features.OpenWriterAt, remote.features.OpenChunkWriter = nil, nil
	remote.id.Store(1)
	opt := vfscommon.Opt
	opt.WriteBack, opt.HandleCaching, opt.CachePollInterval = 0, 0, 0
	opt.FastFingerprint = true
	c, err := New(ctx, remote, &opt, nil)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); require.NoError(t, c.CleanUp()) })
	r.WriteObject(ctx, "created", "old", time.Unix(1, 0))
	original, err := remote.NewObject(ctx, "created")
	require.NoError(t, err)
	snapshot := &createdOwnerSnapshotID{Object: cacheSnapshot(t, original), id: "1"}
	item := c.Item("created")
	require.NoError(t, item.Open(original))
	require.NoError(t, item.Close(nil))
	require.NoError(t, original.Remove(ctx))
	remote.id.Store(2)
	_, err = item.OpenWithObject(nil, false)
	require.ErrorIs(t, err, os.ErrNotExist)
	accepted, err := item.OpenWithObject(snapshot, true)
	require.NoError(t, err)
	require.Nil(t, accepted)
	_, err = item.WriteAt([]byte("new-data"), 0)
	require.NoError(t, err)
	accepted, err = item.OpenWithObject(snapshot, false)
	require.NoError(t, err)
	assert.Nil(t, accepted, "stale snapshot must not supersede committed local creation with deleted ID")
	if accepted != nil {
		t.Logf("reaccepted old ID=%s", accepted.(fs.IDer).ID())
	}
	require.NoError(t, item.Close(nil))
	err = item.Close(nil)
	assert.NoError(t, err, "fully present new local data should upload through a new target")
	t.Logf("final writeback: %v", err)
	_, err = remote.NewObject(ctx, "created")
	assert.NoError(t, err, "authorized creation must reach the remote")
}

func TestMissingRangesRecoverFromFreshRemote(t *testing.T) {
	for _, nilTarget := range []bool{false, true} {
		t.Run(map[bool]string{false: "original-source", true: "absent-target"}[nilTarget], func(t *testing.T) {
			ctx := context.Background()
			r, c := newItemTestCache(t)
			r.WriteObject(ctx, "recovery", "abcdefghij", time.Unix(1, 0))
			o, err := r.Fremote.NewObject(ctx, "recovery")
			require.NoError(t, err)
			snapshot := cacheSnapshot(t, o)
			item := c.Item("recovery")
			_, err = item.OpenWithObject(snapshot, false)
			require.NoError(t, err)
			require.NoError(t, o.Remove(ctx))
			if nilTarget {
				_, err = item.OpenWithObject(nil, false)
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			_, err = item.WriteAt([]byte("X"), 0)
			require.NoError(t, err)
			if nilTarget {
				_, err = item.OpenWithObject(snapshot, false)
				require.NoError(t, err)
				require.Nil(t, item.o)
				require.NoError(t, item.Close(nil))
			}
			require.ErrorIs(t, item.store(ctx, nil), fs.ErrorObjectNotFound, "direct store must reject incomplete ranges")
			require.Error(t, item.Close(nil))
			require.Zero(t, item.opens)
			require.Nil(t, item.fd)
			require.Nil(t, item.closing)
			require.Nil(t, item.downloaders)
			require.True(t, item.IsDirty())
			require.Equal(t, int64(1), item.info.Rs.Size())
			_, err = r.Fremote.NewObject(ctx, "recovery")
			require.ErrorIs(t, err, fs.ErrorObjectNotFound)
			r.WriteObject(ctx, "recovery", "abcdefghij", time.Unix(1, 0))
			_, err = item.OpenWithObject(snapshot, false)
			require.NoError(t, err)
			require.NoError(t, item.Close(nil))
			checkObject(t, r, "recovery", "Xbcdefghij")
		})
	}
}

func TestSourceRecoveryDoesNotUseDestinationFingerprint(t *testing.T) {
	ctx := context.Background()
	r, c := newItemTestCache(t)
	r.WriteObject(ctx, "source", "abcdefghij", time.Unix(1, 0))
	o, err := r.Fremote.NewObject(ctx, "source")
	require.NoError(t, err)
	snapshot := cacheSnapshot(t, o)
	item := c.Item("source")
	_, err = item.OpenWithObject(snapshot, false)
	require.NoError(t, err)
	originalFingerprint := item.info.Fingerprint
	require.NoError(t, o.Remove(ctx))
	_, err = item.OpenWithObject(nil, false)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = item.WriteAt([]byte("X"), 0)
	require.NoError(t, err)
	r.WriteObject(ctx, "source", "replacement", time.Unix(2, 0))
	_, err = item.OpenWithObject(snapshot, false)
	require.NoError(t, err)
	item.setModTime(time.Unix(3, 0)) // refreshes the destination fingerprint
	require.NotEqual(t, originalFingerprint, item.info.Fingerprint)
	_, err = item.OpenWithObject(snapshot, false)
	require.NoError(t, err)
	require.True(t, item.sourceMissing)
	require.Equal(t, originalFingerprint, item.sourceFingerprint)
	_, err = item.ReadAt(make([]byte, 10), 0)
	require.ErrorIs(t, err, fs.ErrorObjectNotFound)
	require.Equal(t, int64(1), item.info.Rs.Size(), "replacement bytes cannot fill the lost original source")
	require.NoError(t, item.Close(nil))
	require.NoError(t, item.Close(nil))
	require.Error(t, item.Close(nil))
	checkObject(t, r, "source", "replacement")
	require.True(t, item.IsDirty())
}

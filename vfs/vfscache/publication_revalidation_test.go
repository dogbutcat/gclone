// Regression coverage for real remote changes during upload publication.
package vfscache

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Path-based local objects cannot expose a stale Drive-style update target.
// Keep real local data operations, but reject updates through a retired ID.
type publicationIDFS struct {
	fs.Fs
	id       atomic.Int64
	features fs.Features
}

type publicationIDObject struct {
	fs.Object
	owner *publicationIDFS
	id    int64
}

func (f *publicationIDFS) Features() *fs.Features { return &f.features }
func (f *publicationIDFS) NewObject(ctx context.Context, name string) (fs.Object, error) {
	o, err := f.Fs.NewObject(ctx, name)
	if err != nil {
		return nil, err
	}
	return &publicationIDObject{Object: o, owner: f, id: f.id.Load()}, nil
}
func (o *publicationIDObject) Fs() fs.Info { return o.owner }
func (o *publicationIDObject) ID() string  { return fmt.Sprint(o.id) }
func (o *publicationIDObject) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	if current := o.owner.id.Load(); current != o.id {
		return fmt.Errorf("object ID %d was deleted (current ID %d): %w", o.id, current, fs.ErrorObjectNotFound)
	}
	return o.Object.Update(ctx, in, src, options...)
}

func TestPublishingOpenAdoptsReplacementID(t *testing.T) {
	for _, sameMetadata := range []bool{false, true} {
		t.Run(map[bool]string{false: "different-metadata", true: "identical-metadata"}[sameMetadata], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := fstest.NewRun(t)
			remote := &publicationIDFS{Fs: r.Fremote, features: *r.Fremote.Features()}
			remote.features.Copy, remote.features.Move = nil, nil
			remote.features.OpenWriterAt, remote.features.OpenChunkWriter = nil, nil
			remote.id.Store(1)
			opt := vfscommon.Opt
			opt.WriteBack, opt.HandleCaching, opt.CachePollInterval = 0, 0, 0
			c, err := New(ctx, remote, &opt, nil)
			require.NoError(t, err)
			t.Cleanup(func() { cancel(); require.NoError(t, c.CleanUp()) })
			r.WriteObject(ctx, "wal", "old", time.Unix(1, 0))
			o, err := remote.NewObject(ctx, "wal")
			require.NoError(t, err)
			item := c.Item("wal")
			require.NoError(t, item.Open(o))
			_, err = item.WriteAt([]byte("uploaded bytes"), 0)
			require.NoError(t, err)
			var accepted fs.Object
			require.NoError(t, item.Close(func(uploaded fs.Object) {
				content, mtime := "B", time.Unix(2, 0)
				if sameMetadata {
					content, mtime = "uploaded bytes", uploaded.ModTime(ctx)
				}
				r.WriteObject(ctx, "wal", content, mtime)
				remote.id.Store(2)
				fresh, lookupErr := remote.NewObject(ctx, "wal")
				require.NoError(t, lookupErr)
				if sameMetadata {
					require.Equal(t, fs.Fingerprint(ctx, uploaded, false), fs.Fingerprint(ctx, fresh, false))
				}
				accepted, err = item.OpenWithObject(fresh, false)
				require.NoError(t, err)
				buf := make([]byte, len("uploaded bytes"))
				_, err = item.ReadAt(buf, 0)
				require.NoError(t, err)
				assert.Equal(t, "uploaded bytes", string(buf), "dirty local bytes survive target replacement")
			}))
			require.Equal(t, "2", accepted.(fs.IDer).ID())
			_, err = item.WriteAt([]byte("pending bytes!"), 0)
			require.NoError(t, err)
			require.NoError(t, item.Close(nil), "writeback must target the replacement ID")
			checkObject(t, r, "wal", "pending bytes!")
		})
	}
}

func TestExistingCacheOpenRejectsConfirmedDeletion(t *testing.T) {
	for _, absentSnapshot := range []bool{false, true} {
		t.Run(map[bool]string{false: "old-object", true: "nil-object"}[absentSnapshot], func(t *testing.T) {
			_, c, item, old := uploadedCacheFixture(t)
			require.NoError(t, item.o.Remove(context.Background()))
			if absentSnapshot {
				old = nil
			}
			data, err := os.ReadFile(c.toOSPath("wal"))
			require.NoError(t, err)
			meta, err := os.ReadFile(c.toOSPathMeta("wal"))
			require.NoError(t, err)
			object, fingerprint := item.o, item.info.Fingerprint
			for range 2 {
				_, err = item.OpenWithObject(old, false)
				require.ErrorIs(t, err, os.ErrNotExist)
				assert.Zero(t, item.opens)
				assert.Same(t, object, item.o)
				assert.Equal(t, fingerprint, item.info.Fingerprint)
			}
			after, err := os.ReadFile(c.toOSPath("wal"))
			require.NoError(t, err)
			afterMeta, err := os.ReadFile(c.toOSPathMeta("wal"))
			require.NoError(t, err)
			assert.Equal(t, data, after)
			assert.Equal(t, meta, afterMeta)
		})
	}
}

func TestPublishingLookupErrorPreservesData(t *testing.T) {
	_, c, item, old := uploadedCacheFixture(t)
	lookup := c.fremote.(*cacheLookupProbe)
	require.NoError(t, item.Open(item.o))
	_, err := item.WriteAt([]byte("pending bytes!"), 0)
	require.NoError(t, err)
	require.NoError(t, item.Close(func(uploaded fs.Object) {
		lookup.override, lookup.err = true, os.ErrPermission
		_, openErr := item.OpenWithObject(old, false)
		require.ErrorIs(t, openErr, os.ErrPermission)
		assert.Zero(t, item.opens)
		assert.Same(t, uploaded, item.o)
		data, readErr := os.ReadFile(c.toOSPath("wal"))
		require.NoError(t, readErr)
		assert.Equal(t, "pending bytes!", string(data))
		assert.True(t, item.publishing)
	}))
	assert.False(t, item.publishing)
}

// Absence recovery regression: a successful revalidation must keep the new backend identity.
type absenceIDSnapshot struct {
	fs.Object
	id string
}

func (o *absenceIDSnapshot) ID() string { return o.id }

func TestAbsenceRecoveryRejectsOldEqualFingerprint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := fstest.NewRun(t)
	remote := &publicationIDFS{Fs: r.Fremote, features: *r.Fremote.Features()}
	remote.features.Copy, remote.features.Move = nil, nil
	remote.features.OpenWriterAt, remote.features.OpenChunkWriter = nil, nil
	remote.id.Store(1)
	opt := vfscommon.Opt
	opt.FastFingerprint = true
	opt.WriteBack, opt.HandleCaching, opt.CachePollInterval = 0, 0, 0
	c, err := New(ctx, remote, &opt, nil)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); require.NoError(t, c.CleanUp()) })
	r.WriteObject(ctx, "wal", "old", time.Unix(1, 0))
	o, err := remote.NewObject(ctx, "wal")
	require.NoError(t, err)
	item := c.Item("wal")
	require.NoError(t, item.Open(o))
	_, err = item.WriteAt([]byte("uploaded bytes"), 0)
	require.NoError(t, err)
	var snapshot fs.Object
	require.NoError(t, item.Close(func(uploaded fs.Object) {
		snapshot = &absenceIDSnapshot{Object: cacheSnapshot(t, uploaded), id: uploaded.(fs.IDer).ID()}
		require.NoError(t, uploaded.Remove(ctx))
		_, openErr := item.OpenWithObject(snapshot, false)
		require.ErrorIs(t, openErr, os.ErrNotExist)
		r.WriteObject(ctx, "wal", "uploaded bytes", snapshot.ModTime(ctx))
		remote.id.Store(2)
		replacement, lookupErr := remote.NewObject(ctx, "wal")
		require.NoError(t, lookupErr)
		require.Equal(t, fs.Fingerprint(ctx, snapshot, true), fs.Fingerprint(ctx, replacement, true))
		accepted, openErr := item.OpenWithObject(snapshot, false)
		require.NoError(t, openErr)
		require.Equal(t, "2", accepted.(fs.IDer).ID())
	}))
	require.False(t, item.publishing)
	require.False(t, item.info.Dirty)
	require.Equal(t, "2", item.o.(fs.IDer).ID())
	accepted, err := item.OpenWithObject(snapshot, false)
	require.NoError(t, err)
	t.Logf("after absence, fresh recovery B and callback drain: accepted ID=%s", accepted.(fs.IDer).ID())
	assert.Equal(t, "2", accepted.(fs.IDer).ID(), "an old equal-fingerprint snapshot must not undo validated replacement ownership")
	_, err = item.WriteAt([]byte("pending bytes!"), 0)
	require.NoError(t, err)
	require.NoError(t, item.Close(nil))
	require.NoError(t, item.Close(nil))
	checkObject(t, r, "wal", "pending bytes!")
}

// Regression coverage for cache invalidation from stale File/backend snapshots.
package vfscache

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cacheObjectSnapshot struct {
	fs.Object
	size   int64
	mtime  time.Time
	digest string
}

func (o *cacheObjectSnapshot) Size() int64                       { return o.size }
func (o *cacheObjectSnapshot) ModTime(context.Context) time.Time { return o.mtime }
func (o *cacheObjectSnapshot) Hash(_ context.Context, typ hash.Type) (string, error) {
	if typ == hash.MD5 {
		return o.digest, nil
	}
	return "", hash.ErrUnsupported
}

func cacheSnapshot(t *testing.T, o fs.Object) fs.Object {
	t.Helper()
	digest, err := o.Hash(context.Background(), hash.MD5)
	require.NoError(t, err)
	return &cacheObjectSnapshot{Object: o, size: o.Size(), mtime: o.ModTime(context.Background()), digest: digest}
}

type cacheLookupProbe struct {
	fs.Fs
	calls    int
	override bool
	object   fs.Object
	err      error
}

func (f *cacheLookupProbe) NewObject(ctx context.Context, name string) (fs.Object, error) {
	f.calls++
	if f.override {
		return f.object, f.err
	}
	return f.Fs.NewObject(ctx, name)
}

func uploadedCacheFixture(t *testing.T) (*fstest.Run, *Cache, *Item, fs.Object) {
	t.Helper()
	r := fstest.NewRun(t)
	opt := vfscommon.Opt
	opt.WriteBack, opt.HandleCaching, opt.CachePollInterval = 0, 0, 0
	ctx, cancel := context.WithCancel(context.Background())
	c, err := New(ctx, &cacheLookupProbe{Fs: r.Fremote}, &opt, nil)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); require.NoError(t, c.CleanUp()) })
	r.WriteObject(context.Background(), "wal", "", time.Unix(1, 0))
	o, err := r.Fremote.NewObject(context.Background(), "wal")
	require.NoError(t, err)
	old := cacheSnapshot(t, o)
	item := c.Item("wal")
	require.NoError(t, item.Open(o))
	_, err = item.WriteAt([]byte("uploaded bytes"), 0)
	require.NoError(t, err)
	require.NoError(t, item.Close(nil))
	require.False(t, item.IsDirty())
	return r, c, item, old
}

func TestCacheOpenRevalidatesStaleSnapshot(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(map[bool]string{false: "old-object", true: "absent-object"}[absent], func(t *testing.T) {
			_, c, item, old := uploadedCacheFixture(t)
			lookup := c.fremote.(*cacheLookupProbe)
			lookup.calls = 0
			if absent {
				old = nil
			}
			resolved, err := item.OpenWithObject(old, false)
			require.NoError(t, err)
			require.NotNil(t, resolved)
			assert.Equal(t, int64(14), resolved.Size())
			assert.Equal(t, 1, lookup.calls)
			buf := make([]byte, 14)
			n, err := item.ReadAt(buf, 0)
			require.NoError(t, err)
			assert.Equal(t, 14, n)
			assert.Equal(t, "uploaded bytes", string(buf))
			require.NoError(t, item.Close(nil))
		})
	}
}

func TestCacheMatchingSnapshotDoesNotLookup(t *testing.T) {
	_, c, item, _ := uploadedCacheFixture(t)
	lookup := c.fremote.(*cacheLookupProbe)
	lookup.calls, lookup.override, lookup.err = 0, true, os.ErrPermission
	require.NoError(t, item.Open(item.o))
	assert.Zero(t, lookup.calls)
	require.NoError(t, item.Close(nil))
}

func TestCacheRevalidationErrorPreservesData(t *testing.T) {
	for _, lookupErr := range []error{os.ErrPermission, context.DeadlineExceeded, nil} {
		t.Run(stringifyLookupError(lookupErr), func(t *testing.T) {
			_, c, item, old := uploadedCacheFixture(t)
			before, err := os.ReadFile(c.toOSPath("wal"))
			require.NoError(t, err)
			meta, err := os.ReadFile(c.toOSPathMeta("wal"))
			require.NoError(t, err)
			fingerprint, object := item.info.Fingerprint, item.o
			lookup := c.fremote.(*cacheLookupProbe)
			lookup.override, lookup.err = true, lookupErr
			err = item.Open(old)
			require.Error(t, err)
			if lookupErr != nil {
				assert.ErrorIs(t, err, lookupErr)
			}
			after, err := os.ReadFile(c.toOSPath("wal"))
			require.NoError(t, err)
			afterMeta, err := os.ReadFile(c.toOSPathMeta("wal"))
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Equal(t, meta, afterMeta)
			assert.Equal(t, fingerprint, item.info.Fingerprint)
			assert.Same(t, object, item.o)
			assert.Zero(t, item.opens)
		})
	}
}

func stringifyLookupError(err error) string {
	if err == nil {
		return "invalid-nil-object"
	}
	return err.Error()
}

func TestCacheRealExternalChangesRemainVisible(t *testing.T) {
	for _, change := range []string{"same-size", "truncate", "delete"} {
		t.Run(change, func(t *testing.T) {
			r, c, item, _ := uploadedCacheFixture(t)
			var supplied fs.Object
			expected := ""
			if change == "delete" {
				require.NoError(t, item.o.Remove(context.Background()))
			} else {
				if change == "same-size" {
					expected = "external bytes"
				}
				r.WriteObject(context.Background(), "wal", expected, time.Unix(200, 0))
				var err error
				supplied, err = r.Fremote.NewObject(context.Background(), "wal")
				require.NoError(t, err)
			}
			require.NoError(t, item.Open(supplied))
			size, err := item.GetSize()
			require.NoError(t, err)
			assert.Equal(t, int64(len(expected)), size)
			if len(expected) > 0 {
				buf := make([]byte, len(expected))
				_, err = item.ReadAt(buf, 0)
				require.NoError(t, err)
				assert.Equal(t, expected, string(buf))
			}
			require.NoError(t, item.Close(nil))
			if change == "delete" {
				_, err := c.fremote.NewObject(context.Background(), "wal")
				assert.ErrorIs(t, err, fs.ErrorObjectNotFound)
			}
		})
	}
}

func TestDirtyOpenCannotReplacePublishingObject(t *testing.T) {
	r, c := newItemTestCache(t)
	r.WriteObject(context.Background(), "wal", "", time.Unix(1, 0))
	o, err := r.Fremote.NewObject(context.Background(), "wal")
	require.NoError(t, err)
	old := cacheSnapshot(t, o)
	item := c.Item("wal")
	require.NoError(t, item.Open(o))
	_, err = item.WriteAt([]byte("uploaded bytes"), 0)
	require.NoError(t, err)
	var callbackErr error
	require.NoError(t, item.Close(func(fresh fs.Object) {
		resolved, err := item.OpenWithObject(old, false)
		callbackErr = err
		if err == nil && fs.Fingerprint(context.Background(), resolved, false) != fs.Fingerprint(context.Background(), fresh, false) {
			callbackErr = errors.New("old open replaced the publishing object")
		}
	}))
	require.NoError(t, callbackErr)
	buf := make([]byte, 14)
	_, err = item.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, "uploaded bytes", string(buf))
	require.NoError(t, item.Close(nil))
}

func TestDirtyOpenAdoptsExternalWritebackTarget(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "deleted"}[deleted], func(t *testing.T) {
			r, _, item, _ := uploadedCacheFixture(t)
			require.NoError(t, item.Open(item.o))
			_, err := item.WriteAt([]byte("pending bytes!"), 0)
			require.NoError(t, err)
			var target fs.Object
			if deleted {
				require.NoError(t, item.o.Remove(context.Background()))
			} else {
				r.WriteObject(context.Background(), "wal", "external bytes", time.Unix(300, 0))
				target, err = r.Fremote.NewObject(context.Background(), "wal")
				require.NoError(t, err)
			}
			require.NoError(t, item.Open(target))
			assert.Equal(t, target, item.o, "local dirty bytes must not pin a deleted backend ID")
			buf := make([]byte, 14)
			_, err = item.ReadAt(buf, 0)
			require.NoError(t, err)
			assert.Equal(t, "pending bytes!", string(buf))
			require.NoError(t, item.Close(nil))
			require.NoError(t, item.Close(nil))
			checkObject(t, r, "wal", "pending bytes!")
			assert.False(t, item.publishing)
		})
	}
}

// Regression coverage for local publication epochs and opened-object views.
package vfs

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/dirtree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func objectViewFixture(t *testing.T) (*VFS, *Dir, *File, fs.Object, fs.Object) {
	t.Helper()
	opt := vfscommon.Opt
	opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, 0
	opt.HandleCaching, opt.CachePollInterval = 0, 0
	r, v := newTestVFSOpt(t, &opt)
	r.WriteObject(context.Background(), "wal", "old bytes", t1)
	node, err := v.Stat("wal")
	require.NoError(t, err)
	f := node.(*File)
	old := f.getObject()
	r.WriteObject(context.Background(), "wal", "new bytes", t2)
	fresh, err := r.Fremote.NewObject(context.Background(), "wal")
	require.NoError(t, err)
	root, err := v.Root()
	require.NoError(t, err)
	return v, root, f, old, fresh
}

type listingPublicationEntry struct {
	fs.Directory
	once    sync.Once
	publish func()
}

func (e *listingPublicationEntry) Remote() string {
	e.once.Do(e.publish)
	return e.Directory.Remote()
}

func TestListingCannotLosePublicationDuringApply(t *testing.T) {
	for _, sameName := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent-file", true: "directory-replaces-file"}[sameName], func(t *testing.T) {
			_, root, f, _, fresh := objectViewFixture(t)
			name := "unrelated"
			if sameName {
				name = "wal"
			}
			published := make(chan struct{})
			done := make(chan struct{})
			entry := &listingPublicationEntry{Directory: fs.NewDir(name, t1)}
			entry.publish = func() {
				go func() { f.setObject(fresh); close(done) }()
				require.Eventually(t, func() bool { return f.getObject() == fresh }, 5*time.Second, time.Millisecond)
				close(published)
			}
			started := time.Now()
			root.mu.Lock()
			err := root._readDirFromEntries(fs.DirEntries{entry}, nil, started)
			current := root.items["wal"]
			root.mu.Unlock()
			require.NoError(t, err)
			<-published
			assert.Same(t, f, current, "listing must not create a namespace gap before blocked callback completes")
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("publication did not complete")
			}
		})
	}
}

func TestTreeListingCannotRevertLocalPublication(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "older-entry", true: "missing-entry"}[missing], func(t *testing.T) {
			_, root, f, old, fresh := objectViewFixture(t)
			started := time.Now()
			f.setObject(fresh)
			entries := fs.DirEntries{old}
			if missing {
				entries = nil
			}
			root.mu.Lock()
			err := root._readDirFromDirTree(dirtree.DirTree{"": entries}, started)
			current := root.items["wal"]
			root.mu.Unlock()
			require.NoError(t, err)
			assert.Same(t, f, current, "an earlier empty tree must not remove a later publication")
			assert.Same(t, fresh, f.getObject())
			// A subsequent snapshot is authoritative again, including deletion.
			root.mu.Lock()
			err = root._readDirFromDirTree(dirtree.DirTree{"": entries}, time.Now())
			current = root.items["wal"]
			root.mu.Unlock()
			require.NoError(t, err)
			if missing {
				assert.Nil(t, current)
			} else {
				assert.Same(t, old, f.getObject())
			}
		})
	}
}

func TestObjectReconciliationCannotRevertNewerView(t *testing.T) {
	_, _, f, old, fresh := objectViewFixture(t)
	_, revision := f.objectSnapshot()
	f.setObjectNoUpdate(fresh, time.Now())
	f.reconcileObject(old, revision)
	assert.Same(t, fresh, f.getObject())
	_, revision = f.objectSnapshot()
	f.setObject(old)
	f.reconcileObject(fresh, revision)
	assert.Same(t, old, f.getObject())
}

func TestReadReconciliationDoesNotFenceExternalChange(t *testing.T) {
	_, _, f, old, fresh := objectViewFixture(t)
	_, revision := f.objectSnapshot()
	started := time.Now()
	f.reconcileObject(fresh, revision)
	f.setObjectNoUpdate(old, started)
	assert.Same(t, old, f.getObject(), "a read must not look like a newer local write")
	_, revision = f.objectSnapshot()
	f.reconcileObject(old, revision)
	_, after := f.objectSnapshot()
	assert.Equal(t, revision, after, "no-op reconcile must not invalidate another opener's snapshot")
}

func TestOldFileViewReconcilesRevalidatedCache(t *testing.T) {
	opt := vfscommon.Opt
	opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, 0
	opt.HandleCaching, opt.CachePollInterval = 0, 0
	r, v := newTestVFSOpt(t, &opt)
	r.WriteObject(context.Background(), "wal", "", t1)
	oldObject, err := r.Fremote.NewObject(context.Background(), "wal")
	require.NoError(t, err)
	oldSnapshot := &snapshotObject{Object: oldObject, size: 0, mtime: t1, digest: "d41d8cd98f00b204e9800998ecf8427e"}
	w, err := v.OpenFile("wal", os.O_RDWR, 0600)
	require.NoError(t, err)
	_, err = w.Write([]byte("uploaded bytes"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	root, err := v.Root()
	require.NoError(t, err)
	oldFile := newFile(root, "", oldSnapshot, "wal")
	h, err := oldFile.Open(os.O_RDWR)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })
	buf := make([]byte, 14)
	_, err = h.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, "uploaded bytes", string(buf))
	assert.Equal(t, int64(14), oldFile.getObject().Size())
	require.NoError(t, h.Close())
}

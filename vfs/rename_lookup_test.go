// Regression tests for canonical bindings and cancellation boundaries.
package vfs

import (
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscache/writeback"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func renameLookupVFS(t *testing.T) (*VFS, *Dir, func(string, string) *File) {
	t.Helper()
	opt := vfscommon.Opt
	opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, fs.Duration(time.Hour)
	opt.CachePollInterval = 0
	_, v := newTestVFSOpt(t, &opt)
	t.Cleanup(func() {
		for _, entry := range v.cache.Queue()["queue"].([]writeback.QueueInfo) {
			_ = v.cache.QueueSetExpiry(entry.ID, time.Now(), 0)
		}
	})
	root, err := v.Root()
	require.NoError(t, err)
	return v, root, func(name, data string) *File {
		t.Helper()
		h, err := v.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		require.NoError(t, err)
		_, err = h.Write([]byte(data))
		require.NoError(t, err)
		require.NoError(t, h.Close())
		return h.Node().(*File)
	}
}

func renameWaitingIn(t *testing.T, outer, inner string) {
	t.Helper()
	require.Eventually(t, func() bool {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		for _, stack := range strings.Split(string(buf), "\n\n") {
			if strings.Contains(stack, outer) && strings.Contains(stack, inner) {
				return true
			}
		}
		return false
	}, 3*time.Second, time.Millisecond)
}

func TestRenameSourceMovesBeforeNameSnapshot(t *testing.T) {
	v, root, create := renameLookupVFS(t)
	source := create("source", "original bytes")
	dest, err := root.Mkdir("destination")
	require.NoError(t, err)
	dest.mu.Lock()
	var once sync.Once
	release := func() { once.Do(dest.mu.Unlock) }
	defer release()
	done := make(chan error, 1)
	go func() { done <- root.Rename("source", "target", dest) }()
	// The source stat has returned; destination stat is blocked before Name().
	renameWaitingIn(t, "(*Dir).lockRename(", "(*Dir).stat(")
	require.NoError(t, root.Rename("source", "moved", root))
	replacement := create("source", "replacement bytes")
	release()
	select {
	case err = <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("rename did not resume")
	}
	current, err := v.Stat("destination/target")
	require.NoError(t, err)
	assert.True(t, replacement == current, "rename must re-resolve its original source pathname")
	moved, movedErr := v.Stat("moved")
	assert.NoError(t, movedErr, "a completed independent move must retain its destination")
	assert.True(t, source == moved, "the moved source must retain its binding")
	data, readErr := v.ReadFile("destination/target")
	assert.NoError(t, readErr)
	assert.Equal(t, "replacement bytes", string(data))
	t.Logf("target=%q movedError=%v", data, movedErr)
}

func TestRenameCacheWaitCancellation(t *testing.T) {
	opt := vfscommon.Opt
	opt.CacheMode, opt.CachePollInterval = vfscommon.CacheModeFull, 0
	r, v := newTestVFSOpt(t, &opt)
	r.WriteObject(context.Background(), "source", "source bytes", t1)
	node, err := v.Stat("source")
	require.NoError(t, err)
	source := node.(*File)
	root, err := v.Root()
	require.NoError(t, err)
	mutation, err := v.cache.BeginRename("other", "target")
	require.NoError(t, err)
	defer mutation.Close()
	done := make(chan error, 1)
	go func() { done <- root.Rename("source", "target", root) }()
	renameWaitingIn(t, "(*Dir).Rename(", "[select]")
	require.True(t, source.muRW.TryLock(), "cache waiter must release source lifecycle lock")
	source.muRW.Unlock()
	v.cancel()
	select {
	case err = <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("canceled cache waiter did not resume")
	}
	mutation.Close()
}

func TestRenameTargetMovesBeforeNameSnapshot(t *testing.T) {
	v, root, create := renameLookupVFS(t)
	source := create("source", "source bytes")
	target := create("target", "old target bytes")
	source.mu.Lock()
	var once sync.Once
	release := func() { once.Do(source.mu.Unlock) }
	defer release()
	done := make(chan error, 1)
	go func() { done <- root.Rename("source", "target", root) }()
	// Both stat calls completed; source.Name is blocked before target.Name.
	renameWaitingIn(t, "(*Dir).lockRename(", "(*File).Name(")
	require.NoError(t, root.Rename("target", "moved", root))
	create("target", "replacement target bytes")
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("rename did not resume")
	}
	current, err := v.Stat("target")
	require.NoError(t, err)
	assert.True(t, source == current, "rename must replace the requested destination")
	moved, err := v.Stat("moved")
	require.NoError(t, err)
	assert.True(t, target == moved, "independently moved destination must not be overwritten")
	data, err := v.ReadFile("target")
	require.NoError(t, err)
	assert.Equal(t, "source bytes", string(data))
	movedData, err := v.ReadFile("moved")
	require.NoError(t, err)
	assert.Equal(t, "old target bytes", string(movedData))
	t.Logf("target=%q moved=%q", data, movedData)
}

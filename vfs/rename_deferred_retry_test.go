// Regression: revalidate a destination created during a deferred move retry.
package vfs

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscache/writeback"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenameDestinationCreatedDuringDeferredRetry(t *testing.T) {
	for _, tc := range []struct {
		name, requested, created string
		insensitive              bool
	}{
		{"exact", "target", "target", false},
		{"case-alias", "target", "TARGET", true},
		{"unicode-alias", "caf\u00e9", "cafe\u0301", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRenameDeferredDestination(t, tc.requested, tc.created, tc.insensitive)
		})
	}
}

func testRenameDeferredDestination(t *testing.T, requested, created string, insensitive bool) {
	t.Helper()
	opt := vfscommon.Opt
	opt.CaseInsensitive = insensitive
	opt.CacheMode, opt.WriteBack = vfscommon.CacheModeMinimal, fs.Duration(time.Hour)
	opt.HandleCaching, opt.CachePollInterval = 0, 0
	r, v := newTestVFSOpt(t, &opt)
	t.Cleanup(func() {
		for _, entry := range v.cache.Queue()["queue"].([]writeback.QueueInfo) {
			_ = v.cache.QueueSetExpiry(entry.ID, time.Now(), 0)
		}
	})
	features := r.Fremote.Features()
	move := features.Move
	require.NotNil(t, move)
	var attempts atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	t.Cleanup(func() { features.Move = move })
	features.Move = func(ctx context.Context, o fs.Object, remote string) (fs.Object, error) {
		if remote == "middle" {
			switch attempts.Add(1) {
			case 1:
				return nil, errors.New("test transient deferred move failure")
			case 2:
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
		return move(ctx, o, remote)
	}

	source, err := v.OpenFile("original", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	_, err = source.Write([]byte("source bytes"))
	require.NoError(t, err)
	require.NoError(t, v.Rename("original", "middle"))
	require.NoError(t, source.Close())
	require.Equal(t, int32(1), attempts.Load())
	sourceFile := source.Node().(*File)
	sourceFile.mu.RLock()
	pending := sourceFile.pendingRenameFun != nil
	sourceFile.mu.RUnlock()
	require.True(t, pending)

	done := make(chan error, 1)
	go func() { done <- v.Rename("middle", requested) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("rename returned before deferred move barrier: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("deferred move barrier was not reached")
	}
	target, err := v.OpenFile(created, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	_, err = target.Write([]byte("old target bytes"))
	require.NoError(t, err)
	require.NoError(t, target.Sync())
	targetFile := target.Node().(*File)
	unblock()
	select {
	case err = <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("rename failed to finish")
	}
	current, err := v.Stat(requested)
	require.NoError(t, err)
	require.Same(t, sourceFile, current)
	alias, err := v.Stat(created)
	require.NoError(t, err)
	require.Same(t, sourceFile, alias)
	aliasData, err := v.ReadFile(created)
	require.NoError(t, err)
	require.Equal(t, "source bytes", string(aliasData))
	root, err := v.Root()
	require.NoError(t, err)
	nodes, err := root.ReadDirAll()
	require.NoError(t, err)
	require.Len(t, nodes, 1, "only the replacement binding may remain")
	targetFile.mu.RLock()
	retired := targetFile.unlinked
	targetFile.mu.RUnlock()
	t.Logf("overwritten target lifetime retired=%v", retired)
	assert.True(t, retired, "destination created while the old deferred move waited must be retired")
	removeErr := targetFile.Remove()
	t.Logf("retained target File.Remove returned %v", removeErr)
	assert.ErrorIs(t, removeErr, ENOENT, "a retained overwritten File must not remove the replacement")
	data, readErr := v.ReadFile(requested)
	assert.NoError(t, readErr, "replacement must survive old File.Remove")
	assert.Equal(t, "source bytes", string(data))
	require.NoError(t, target.Close())
}

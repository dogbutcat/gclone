// Verify clean source replacement and reject unauthorized background range fills.
package vfscache

import (
	"context"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/ranges"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanReplacementAfterAbsenceIsReadable(t *testing.T) {
	ctx := context.Background()
	r, c := newItemTestCache(t)
	r.WriteObject(ctx, "clean", "abcdefghij", time.Unix(1, 0))
	old, err := r.Fremote.NewObject(ctx, "clean")
	require.NoError(t, err)
	item := c.Item("clean")
	_, err = item.OpenWithObject(old, false)
	require.NoError(t, err)
	require.NoError(t, item.Close(nil))
	require.Zero(t, item.info.Rs.Size())
	require.False(t, item.IsDirty())
	require.NoError(t, old.Remove(ctx))
	_, err = item.OpenWithObject(nil, false)
	require.ErrorIs(t, err, os.ErrNotExist)
	r.WriteObject(ctx, "clean", "replacement", time.Unix(2, 0))
	replacement, err := r.Fremote.NewObject(ctx, "clean")
	require.NoError(t, err)
	_, err = item.OpenWithObject(replacement, false)
	require.NoError(t, err)
	data := make([]byte, len("replacement"))
	n, readErr := item.ReadAt(data, 0)
	assert.NoError(t, readErr, "a clean accepted replacement is its own valid range source")
	assert.Equal(t, len(data), n)
	assert.Equal(t, "replacement", string(data))
	require.NoError(t, item.Close(nil))
}

func TestCachedReadAheadMustNotFillLostSource(t *testing.T) {
	ctx := context.Background()
	r, c := newItemTestCache(t)
	c.opt.ReadAhead = 32
	r.WriteObject(ctx, "prefetch", "abcdefghij", time.Unix(1, 0))
	raw, err := r.Fremote.NewObject(ctx, "prefetch")
	var opens atomic.Int64
	old := &countedSourceObject{Object: raw, opens: &opens}
	require.NoError(t, err)
	snapshot := cacheSnapshot(t, old)
	item := c.Item("prefetch")
	_, err = item.OpenWithObject(snapshot, false)
	require.NoError(t, err)
	require.NoError(t, old.Remove(ctx))
	_, err = item.OpenWithObject(nil, false)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = item.WriteAt([]byte("X"), 0)
	require.NoError(t, err)
	r.WriteObject(ctx, "prefetch", "0123456789", time.Unix(2, 0))
	_, err = item.OpenWithObject(snapshot, false)
	require.NoError(t, err)
	require.Equal(t, int64(1), item.info.Rs.Size())
	data := make([]byte, 1)
	_, err = item.ReadAt(data, 0)
	require.NoError(t, err, "the cached local edit itself must remain readable")
	require.Equal(t, "X", string(data))
	// A late background writer must not supply the unavailable original bytes.
	_, _, err = item.WriteAtNoOverwrite([]byte("123456789"), 1)
	require.ErrorIs(t, err, fs.ErrorObjectNotFound)
	require.Equal(t, int64(1), item.info.Rs.Size())
	require.NoError(t, item.Close(nil))
	closeErr := item.Close(nil)
	t.Logf("final Close: %v; dirty=%v", closeErr, item.IsDirty())
	assert.Error(t, closeErr, "missing original bytes must not become uploadable through prefetch")
	checkObject(t, r, "prefetch", "0123456789")
	assert.True(t, item.IsDirty())
	assert.Zero(t, opens.Load(), "cached reads must not schedule unavailable-source prefetch; Close drained all workers")
	_, err = r.Fremote.NewObject(ctx, "prefetch")
	require.NotErrorIs(t, err, fs.ErrorObjectNotFound)
}

type countedSourceObject struct {
	fs.Object
	opens *atomic.Int64
}

func (o *countedSourceObject) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	o.opens.Add(1)
	return o.Object.Open(ctx, options...)
}

func TestRetiredDownloaderCannotFillAcceptedReplacement(t *testing.T) {
	ctx := context.Background()
	r, c := newItemTestCache(t)
	r.WriteObject(ctx, "epoch", "abcdefghij", time.Unix(1, 0))
	old, err := r.Fremote.NewObject(ctx, "epoch")
	require.NoError(t, err)
	item := c.Item("epoch")
	_, err = item.OpenWithObject(old, false)
	require.NoError(t, err)
	item.mu.Lock()
	oldWriter := &downloadTarget{Item: item, epoch: item.downloadEpoch}
	oldDownloaders := item.downloaders
	item.mu.Unlock()
	require.NoError(t, item.Close(nil))
	require.NoError(t, old.Remove(ctx))
	_, err = item.OpenWithObject(nil, false)
	require.ErrorIs(t, err, os.ErrNotExist)
	r.WriteObject(ctx, "epoch", "replacement", time.Unix(2, 0))
	fresh, err := r.Fremote.NewObject(ctx, "epoch")
	require.NoError(t, err)
	_, err = item.OpenWithObject(fresh, false)
	require.NoError(t, err)
	require.False(t, item.sourceMissing)
	_, _, err = oldWriter.WriteAtNoOverwrite([]byte("old bytes!"), 0)
	require.ErrorIs(t, err, errObjectViewChanged, "old callbacks remain invalid even after source guard clears")
	require.Zero(t, item.info.Rs.Size())
	done := make(chan error, 1)
	go func() { done <- oldDownloaders.Download(ranges.Range{Pos: 0, Size: 1}) }()
	select {
	case err = <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("a closed downloader accepted a waiter after its dispatcher exited")
	}
	buf := make([]byte, len("replacement"))
	_, err = item.ReadAt(buf, 0)
	require.NoError(t, err)
	require.Equal(t, "replacement", string(buf))
	require.NoError(t, item.Close(nil))
}

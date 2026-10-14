// Preserve acknowledged foreground writes during downloader retirement.
package vfscache

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mutationCloseObject struct {
	fs.Object
	entered chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (o *mutationCloseObject) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	r, err := o.Object.Open(ctx, options...)
	if err != nil {
		return nil, err
	}
	return &mutationCloseReader{ReadCloser: r, owner: o}, nil
}

type mutationCloseReader struct {
	io.ReadCloser
	owner *mutationCloseObject
}

func (r *mutationCloseReader) Close() error {
	r.owner.once.Do(func() { close(r.owner.entered) })
	<-r.owner.release
	return r.ReadCloser.Close()
}

func TestDirtyWriteDuringDownloaderDrainSurvives(t *testing.T) {
	ctx := context.Background()
	r, c := newItemTestCache(t)
	r.WriteObject(ctx, "drain", "abcdefghij", time.Unix(1, 0))
	raw, err := r.Fremote.NewObject(ctx, "drain")
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(finish)
	old := &mutationCloseObject{Object: raw, entered: entered, release: release, once: &enterOnce}
	item := c.Item("drain")
	_, err = item.OpenWithObject(old, false)
	require.NoError(t, err)
	readDone := make(chan error, 1)
	go func() { _, e := item.ReadAt(make([]byte, 1), 0); readDone <- e }()
	select {
	case err = <-readDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("initial read did not complete")
	}
	r.WriteObject(ctx, "drain", "replacement", time.Unix(2, 0))
	fresh, err := r.Fremote.NewObject(ctx, "drain")
	require.NoError(t, err)
	openDone := make(chan error, 1)
	go func() { _, e := item.OpenWithObject(fresh, false); openDone <- e }()
	require.Eventually(t, func() bool { item.mu.Lock(); defer item.mu.Unlock(); return item.sourceChanging }, 5*time.Second, time.Millisecond, "replacement must reach the new downloader-drain unlock")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("downloader Close did not enter barrier")
	}
	n, err := item.WriteAt([]byte("LOCAL"), 0)
	require.NoError(t, err)
	require.Equal(t, 5, n)
	require.True(t, item.IsDirty())
	finish()
	select {
	case err = <-openDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("replacement open did not finish")
	}
	t.Logf("after successful foreground write and replacement open: dirty=%v", item.IsDirty())
	assert.True(t, item.IsDirty(), "invalidation must recheck foreground dirty ownership after draining")
	_, statErr := os.Stat(c.toOSPath("drain"))
	assert.NoError(t, statErr, "new dirty data must not be unlinked by clean-cache invalidation")
	item.mu.Lock()
	data := make([]byte, 5)
	_, readErr := item.fd.ReadAt(data, 0)
	item.mu.Unlock()
	require.NoError(t, readErr)
	require.Equal(t, "LOCAL", string(data))
	require.NoError(t, item.Close(nil))
	require.NoError(t, item.Close(nil))
	checkObject(t, r, "drain", "LOCALfghij")
}

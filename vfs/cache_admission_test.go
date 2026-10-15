// A deferred VFS open reacquires an evicted cache binding before cache-path mutation.
package vfs

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/require"
)

func TestDeferredOpenReacquiresCurrentDirtyCache(t *testing.T) {
	ctx := context.Background()
	r := fstest.NewRun(t)
	r.WriteObject(ctx, "file", "old", time.Unix(1, 0))
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.CachePollInterval, opt.HandleCaching, opt.WriteBack = 0, 0, 0
	v := New(ctx, r.Fremote, &opt)
	t.Cleanup(func() { cleanupVFS(t, v) })
	deferred, err := v.OpenFile("file", os.O_RDWR, 0600)
	require.NoError(t, err)
	old := deferred.(*RWFileHandle).item
	v.cache.Remove("file")
	writer, err := v.OpenFile("file", os.O_RDWR, 0600)
	require.NoError(t, err)
	current := writer.(*RWFileHandle).item
	require.NotSame(t, old, current)
	_, err = writer.WriteAt([]byte("new"), 0)
	require.NoError(t, err)
	data := make([]byte, 3)
	_, err = deferred.ReadAt(data, 0)
	require.NoError(t, err)
	require.Equal(t, "new", string(data))
	require.Same(t, current, deferred.(*RWFileHandle).item)
	require.NoError(t, deferred.Close())
	require.NoError(t, writer.Close())
	object, err := r.Fremote.NewObject(ctx, "file")
	require.NoError(t, err)
	in, err := object.Open(ctx)
	require.NoError(t, err)
	content, err := io.ReadAll(in)
	require.NoError(t, err)
	require.NoError(t, in.Close())
	require.Equal(t, "new", string(content))
}

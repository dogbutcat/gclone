// Regression coverage for creation intent after cache revalidation finds deletion.
package vfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAfterConfirmedRemoteDeletion(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		for _, grace := range []time.Duration{0, 30 * time.Millisecond} {
			for _, flags := range []int{os.O_RDONLY, os.O_RDWR, os.O_RDONLY | os.O_CREATE, os.O_RDWR | os.O_CREATE} {
				if readOnly && flags&os.O_RDWR != 0 {
					continue
				}
				t.Run(fmt.Sprintf("readonly=%v/grace=%s/flags=%s", readOnly, grace, decodeOpenFlags(flags)), func(t *testing.T) {
					ctx := context.Background()
					opt := vfscommon.Opt
					opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, 0
					opt.HandleCaching, opt.CachePollInterval = fs.Duration(grace), 0
					opt.ReadOnly = readOnly
					r, v := newTestVFSOpt(t, &opt)
					r.WriteObject(ctx, "wal", "old bytes", t1)
					h, err := v.OpenFile("wal", os.O_RDONLY, 0)
					require.NoError(t, err)
					_, err = io.ReadAll(h)
					require.NoError(t, err)
					require.NoError(t, h.Close())
					r.WriteObject(ctx, "wal", "replacement bytes", t2)
					root, err := v.Root()
					require.NoError(t, err)
					require.NoError(t, root.readDir())
					h, err = v.OpenFile("wal", flags, 0600)
					require.NoError(t, err)
					t.Cleanup(func() { _ = h.Close() })
					obj, err := r.Fremote.NewObject(ctx, "wal")
					require.NoError(t, err)
					require.NoError(t, obj.Remove(ctx))
					for range 2 {
						data, readErr := io.ReadAll(h)
						assert.Empty(t, data)
						if flags&os.O_CREATE != 0 && !readOnly {
							require.NoError(t, readErr)
						} else {
							require.ErrorIs(t, readErr, ENOENT)
						}
					}
					require.NoError(t, h.Close())
					if flags&os.O_CREATE != 0 && !readOnly {
						// Creation must persist even when the caller never writes.
						obj, err = r.Fremote.NewObject(ctx, "wal")
						require.NoError(t, err)
						assert.Zero(t, obj.Size())
					} else {
						require.Eventually(t, func() bool {
							if root.readDir() != nil {
								return false
							}
							_, err = v.Stat("wal")
							return os.IsNotExist(err)
						}, 5*time.Second, 5*time.Millisecond, "read-only/deferred opens must not create virtual ghosts")
						_, err = r.Fremote.NewObject(ctx, "wal")
						require.ErrorIs(t, err, fs.ErrorObjectNotFound)
					}
				})
			}
		}
	}
}

func TestCreateEmptyFileAfterCachedDeletion(t *testing.T) {
	ctx := context.Background()
	opt := vfscommon.Opt
	opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, 0
	opt.HandleCaching, opt.CachePollInterval = 0, 0
	r, v := newTestVFSOpt(t, &opt)
	r.WriteObject(ctx, "wal", "cached bytes", t1)
	h, err := v.OpenFile("wal", os.O_RDONLY, 0)
	require.NoError(t, err)
	_, err = io.ReadAll(h)
	require.NoError(t, err)
	require.NoError(t, h.Close())
	obj, err := r.Fremote.NewObject(ctx, "wal")
	require.NoError(t, err)
	require.NoError(t, obj.Remove(ctx))
	root, err := v.Root()
	require.NoError(t, err)
	require.NoError(t, root.readDir())
	// A new File has no remote object but shares the old pathname's clean
	// cache history. Explicit creation must remain different from a stale read.
	h, err = v.OpenFile("wal", os.O_RDONLY|os.O_CREATE, 0600)
	require.NoError(t, err)
	data, err := io.ReadAll(h)
	require.NoError(t, err)
	assert.Empty(t, data)
	require.NoError(t, h.Close())
	obj, err = r.Fremote.NewObject(ctx, "wal")
	require.NoError(t, err)
	assert.Zero(t, obj.Size())
}

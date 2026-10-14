// Verify Linux FUSE identity across pathname changes and directory enumeration.
//go:build linux

package mount

import (
	"context"
	"os"
	"testing"
	"time"

	"bazil.org/fuse"
	"github.com/dogbutcat/gclone/cmd/mountlib"
	"github.com/dogbutcat/gclone/vfs"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

func TestNodeIdentityAcrossRenameAndRecreation(t *testing.T) {
	ctx := context.Background()
	remote, err := fs.NewFs(ctx, t.TempDir())
	require.NoError(t, err)
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.CachePollInterval, opt.HandleCaching, opt.WriteBack = 0, 0, 0
	v := vfs.New(ctx, remote, &opt)
	t.Cleanup(func() {
		v.WaitForWriters(10 * time.Second)
		v.Shutdown()
		require.NoError(t, v.CleanUp())
	})
	root, err := v.Root()
	require.NoError(t, err)
	d := &Dir{Dir: root, fsys: NewFS(v, &mountlib.Opt)}
	create := func(name string) *File {
		t.Helper()
		h, err := v.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
		require.NoError(t, err)
		_, err = h.Write([]byte(name))
		require.NoError(t, err)
		require.NoError(t, h.Close())
		n, err := d.Lookup(ctx, &fuse.LookupRequest{Name: name}, &fuse.LookupResponse{})
		require.NoError(t, err)
		return n.(*File)
	}
	attr := func(n *File) fuse.Attr {
		t.Helper()
		var a fuse.Attr
		require.NoError(t, n.Attr(ctx, &a))
		return a
	}
	source, victim := create("source"), create("target")
	sourceBefore, victimBefore := attr(source), attr(victim)
	require.NotZero(t, sourceBefore.Inode, "pathname-derived fallback changes identity after rename")
	require.NotEqual(t, sourceBefore.Inode, victimBefore.Inode)
	require.NoError(t, v.Rename("source", "target"))
	n, err := d.Lookup(ctx, &fuse.LookupRequest{Name: "target"}, &fuse.LookupResponse{})
	require.NoError(t, err)
	require.Equal(t, sourceBefore.Inode, attr(n.(*File)).Inode)
	require.Equal(t, victimBefore.Inode, attr(victim).Inode)
	fresh := create("source")
	require.NotEqual(t, sourceBefore.Inode, attr(fresh).Inode)
	entries, err := d.ReadDirAll(ctx)
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.Name == "target" {
			require.Equal(t, sourceBefore.Inode, entry.Inode, "readdir and stat identify the same file")
		}
	}
}

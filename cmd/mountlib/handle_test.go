// Verify cache lifetimes accepted by OS mounts survive namespace retirement.
package mountlib

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

func TestPreparedHandleSurvivesRetirement(t *testing.T) {
	for _, action := range []string{"unlink", "replace"} {
		t.Run(action, func(t *testing.T) {
			remote, err := fs.NewFs(context.Background(), t.TempDir())
			require.NoError(t, err)
			opt := vfscommon.Opt
			opt.CacheMode = vfscommon.CacheModeFull
			opt.CachePollInterval, opt.HandleCaching, opt.WriteBack = 0, 0, 0
			view := vfs.New(context.Background(), remote, &opt)
			t.Cleanup(func() { view.WaitForWriters(5 * time.Second); view.Shutdown(); require.NoError(t, view.CleanUp()) })
			create := func(name, body string) {
				h, err := view.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
				require.NoError(t, err)
				_, err = h.Write([]byte(body))
				require.NoError(t, err)
				require.NoError(t, h.Close())
			}
			create("shm", "old data")
			old, err := view.OpenFile("shm", os.O_RDWR, 0600)
			require.NoError(t, err)
			require.NoError(t, PrepareHandle(old)) // No read or write before unlink.
			defer old.Close()
			if action == "unlink" {
				require.NoError(t, old.Node().Remove())
				create("shm", "new data")
			} else {
				create("temporary", "new data")
				require.NoError(t, view.Rename("temporary", "shm"))
			}
			_, err = old.WriteAt([]byte("late"), 0)
			require.NoError(t, err)
			require.NoError(t, old.Sync())
			require.NoError(t, old.Close())
			got, err := view.ReadFile("shm")
			require.NoError(t, err)
			require.Equal(t, "new data", string(got))
			stale, err := old.Node().Open(os.O_RDWR)
			require.NoError(t, err)
			require.ErrorIs(t, PrepareHandle(stale), vfs.ENOENT)
			_, err = stale.Read(make([]byte, 1))
			require.Error(t, err, "failed preparation must close the attempted stale handle")
		})
	}
}

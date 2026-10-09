// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

// Run the more functional vfstest package on the vfs

package vfs_test

import (
	"testing"

	_ "github.com/dogbutcat/gclone/backend/all" // import all the backends
	"github.com/dogbutcat/gclone/cmd/mountlib"
	"github.com/dogbutcat/gclone/vfs"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/dogbutcat/gclone/vfs/vfstest"
	"github.com/rclone/rclone/fstest"
)

// TestFunctional runs more functional tests all the tests against all the
// VFS cache modes
func TestFunctional(t *testing.T) {
	if *fstest.RemoteName != "" {
		t.Skip("Skip on non local")
	}
	vfstest.RunTests(t, true, vfscommon.CacheModeOff, true, func(VFS *vfs.VFS, mountpoint string, opt *mountlib.Options) (unmountResult <-chan error, unmount func() error, actualMountpoint string, err error) {
		unmountResultChan := make(chan (error), 1)
		unmount = func() error {
			unmountResultChan <- nil
			return nil
		}
		return unmountResultChan, unmount, mountpoint, nil
	})
}

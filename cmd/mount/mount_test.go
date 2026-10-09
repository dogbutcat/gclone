// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build linux

package mount

import (
	"testing"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/dogbutcat/gclone/vfs/vfstest"
)

func TestMount(t *testing.T) {
	vfstest.RunTests(t, false, vfscommon.CacheModeOff, true, mount)
}

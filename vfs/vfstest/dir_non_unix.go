// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build !linux && !darwin && !freebsd

package vfstest

import (
	"runtime"
	"testing"
)

// TestDirRewind checks that re-reading a rewound directory works
func TestDirRewind(t *testing.T) {
	t.Skip("not supported on " + runtime.GOOS)
}

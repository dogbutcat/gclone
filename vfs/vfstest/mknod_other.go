// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build !linux && !darwin && !freebsd && !openbsd

package vfstest

import (
	"runtime"
	"testing"
)

// TestMknod is not supported on this platform.
func TestMknod(t *testing.T) {
	t.Skip("not supported on " + runtime.GOOS)
}

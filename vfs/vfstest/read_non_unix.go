// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build !linux && !darwin && !freebsd

package vfstest

import (
	"runtime"
	"testing"
)

// TestReadFileDoubleClose tests double close on read
func TestReadFileDoubleClose(t *testing.T) {
	t.Skip("not supported on " + runtime.GOOS)
}

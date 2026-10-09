// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build !windows && !plan9 && !js && !noselfupdate

package selfupdate

import (
	"golang.org/x/sys/unix"
)

func writable(path string) bool {
	return unix.Access(path, unix.W_OK) == nil
}

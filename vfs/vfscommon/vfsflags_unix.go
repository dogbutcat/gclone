// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build linux || darwin || freebsd || openbsd

package vfscommon

import (
	"golang.org/x/sys/unix"
)

// get the current umask
func getUmask() int {
	umask := unix.Umask(0) // read the umask
	unix.Umask(umask)      // set it back to what it was
	return umask
}

// get the current uid
func getUID() uint32 {
	return uint32(unix.Geteuid())
}

// get the current gid
func getGID() uint32 {
	return uint32(unix.Getegid())
}

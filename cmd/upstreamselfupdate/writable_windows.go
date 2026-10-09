// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build windows && !noselfupdate

package selfupdate

import (
	"os"
)

func writable(path string) bool {
	info, err := os.Stat(path)
	const UserWritableBit = 128
	if err == nil {
		return info.Mode().Perm()&UserWritableBit != 0
	}
	return false
}

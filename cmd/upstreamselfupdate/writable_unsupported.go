// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build (plan9 || js) && !noselfupdate

package selfupdate

func writable(path string) bool {
	return true
}

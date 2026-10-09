// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build unix && !linux

package nfs

// Turn the diskHandler into a symlink cache
func (dh *diskHandler) makeSymlinkCache() error {
	return ErrorSymlinkCacheNotSupported
}

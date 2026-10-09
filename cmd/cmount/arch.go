// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

package cmount

// ProvidedBy returns true if the rclone build for the given OS
// provides support for lib/cgo-fuse
func ProvidedBy(osName string) bool {
	return osName == "windows" || osName == "darwin"
}

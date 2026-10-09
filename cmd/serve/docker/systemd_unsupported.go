// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build !linux || android

package docker

import (
	"os"
)

//lint:ignore U1000 unused when not building linux
func systemdActivationFiles() []*os.File {
	return nil
}

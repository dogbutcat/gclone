// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build linux && !android

package docker

import (
	"os"

	"github.com/coreos/go-systemd/v22/activation"
	"github.com/coreos/go-systemd/v22/util"
)

func systemdActivationFiles() []*os.File {
	if util.IsRunningSystemd() {
		return activation.Files(false)
	}
	return nil
}

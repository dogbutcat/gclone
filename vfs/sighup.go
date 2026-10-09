// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build !plan9 && !js

package vfs

import (
	"os"
	"os/signal"
	"syscall"
)

// NotifyOnSigHup makes SIGHUP notify given channel on supported systems
func NotifyOnSigHup(sighupChan chan os.Signal) {
	signal.Notify(sighupChan, syscall.SIGHUP)
}

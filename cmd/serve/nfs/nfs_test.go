// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build unix

// The serving is tested in cmd/nfsmount - here we test anything else
package nfs

import (
	"testing"

	"github.com/dogbutcat/gclone/cmd/serve/servetest"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs/rc"
)

func TestRc(t *testing.T) {
	servetest.TestRc(t, rc.Params{
		"type":           "nfs",
		"vfs_cache_mode": "off",
	})
}

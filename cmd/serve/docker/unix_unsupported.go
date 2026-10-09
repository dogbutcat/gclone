// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build !linux && !freebsd

package docker

import (
	"errors"
	"net"
)

func newUnixListener(path string, gid int) (net.Listener, string, error) {
	return nil, "", errors.New("unix sockets require Linux or FreeBSD")
}

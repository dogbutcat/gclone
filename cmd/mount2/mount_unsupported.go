// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

//go:build !linux && (!darwin || !amd64)

// Package mount2 implements a FUSE mounting system for rclone remotes.
//
// Build for mount for unsupported platforms to stop go complaining
// about "no buildable Go source files".
package mount2

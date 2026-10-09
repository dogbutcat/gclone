// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

// Build for nfsmount for unsupported platforms to stop go complaining
// about "no buildable Go source files "

//go:build !unix

// Package nfsmount implements mount command using NFS.
package nfsmount

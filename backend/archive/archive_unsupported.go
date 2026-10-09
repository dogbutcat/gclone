// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

// Build for archive for unsupported platforms to stop go complaining
// about "no buildable Go source files "

//go:build plan9

// Package archive implements a backend to access archive files in a remote
package archive

//go:build !plan9

// Preserve the platform retry errno for superseded cache observations.
package vfscache

import "syscall"

var errObjectViewChanged = syscall.EAGAIN

//go:build plan9

// Plan 9 has string errors and does not define the POSIX EAGAIN errno.
package vfscache

import "errors"

var errObjectViewChanged = errors.New("cache object view changed: retry operation")

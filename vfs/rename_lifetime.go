// gclone-local rename-overwrite lifetime boundary; see vfs/UPSTREAM.txt.
package vfs

import (
	"errors"
	"syscall"

	"github.com/dogbutcat/gclone/vfs/vfscache"
)

// renameCommittedError reports a local failure after a remote move committed.
// Dir.Rename must publish the committed namespace before returning this error.
type renameCommittedError struct{ error }

func (e *renameCommittedError) Unwrap() error { return e.error }

// replaceDestination fences callbacks and deferred opens on the old File.
// The source's muRW is held; never wait for a second File lock (opposing
// renames would deadlock). Cache path reservations are held by the caller.
func (f *File) replaceDestination(d *Dir, name string, action func(*File) (bool, error)) error {
	node, err := d.stat(name)
	if err != nil && !errors.Is(err, ENOENT) {
		return err
	}
	var target *File
	if node != nil {
		var ok bool
		target, ok = node.(*File)
		if !ok {
			return syscall.EISDIR
		}
		if target == f {
			return nil
		}
		if !target.muRW.TryLock() {
			return syscall.EBUSY
		}
		defer target.muRW.Unlock()
		d.mu.Lock()
		target.mu.Lock()
		if d.items[name] != target || target.unlinked || target.unlinking {
			target.mu.Unlock()
			d.mu.Unlock()
			return syscall.EBUSY
		}
		target.unlinking = true
		target.mu.Unlock()
		d.mu.Unlock()
	}
	committed, err := action(target)
	if target != nil {
		var recoveryErr *vfscache.RenameRecoveryError
		if committed || errors.As(err, &recoveryErr) {
			d.unlinkFile(target)
		} else {
			target.mu.Lock()
			target.unlinking = false
			target.mu.Unlock()
		}
	}
	if committed && err != nil {
		return &renameCommittedError{err}
	}
	return err
}

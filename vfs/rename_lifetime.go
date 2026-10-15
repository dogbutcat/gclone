// gclone-local rename-overwrite lifetime boundary; see vfs/UPSTREAM.txt.
package vfs

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/dogbutcat/gclone/vfs/vfscache"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/operations"
)

// renameCommittedError reports a local failure after a remote move committed.
// Dir.Rename must publish the committed namespace before returning this error.
type renameCommittedError struct{ error }

func (e *renameCommittedError) Unwrap() error { return e.error }

var errRenameBindingChanged = errors.New("rename pathname changed before reservation")

// _renameBinding resolves the caller's original name with the same exact-name
// priority and normalization as stat, without I/O. Requires d.mu. A File's Name
// snapshot alone is insufficient: it may already have moved to another name.
func (d *Dir) _renameBinding(leaf string) (Node, error) {
	if node, ok := d.items[leaf]; ok {
		return node, nil
	}
	ci := fs.GetConfig(d.vfs.ctx)
	normUnicode := !ci.NoUnicodeNormalization
	normCase := ci.IgnoreCaseSync || d.vfs.Opt.CaseInsensitive
	if !normUnicode && !normCase {
		return nil, nil
	}
	normalized := operations.ToNormal(leaf, normUnicode, normCase)
	var match Node
	for name, node := range d.items {
		if operations.ToNormal(name, normUnicode, normCase) == normalized {
			if match != nil {
				return nil, fmt.Errorf("duplicate filename %q detected with case/unicode normalization settings", leaf)
			}
			match = node
		}
	}
	return match, nil
}

// lockRename waits for file lifecycle operations before reserving cache paths.
// Lock in inode order so opposing renames cannot deadlock. A waited-on File
// may have moved or been replaced, so retry pathname lookup after any binding
// change. Keep the locks through directory publication, not just the move.
func (d *Dir) lockRename(oldName, newName string, destDir *Dir) (Node, *File, func(), error) {
	for {
		node, err := d.stat(oldName)
		if err != nil {
			return nil, nil, nil, err
		}
		source, ok := node.(*File)
		if !ok {
			return node, nil, func() {}, nil
		}
		dest, err := destDir.stat(newName)
		if err != nil && !errors.Is(err, ENOENT) {
			return nil, nil, nil, err
		}
		target, ok := dest.(*File)
		if dest != nil && !ok {
			return nil, nil, nil, syscall.EISDIR
		}
		// stat also resolves case/Unicode aliases. Validate the actual map
		// keys it resolved, otherwise a valid alias would retry forever.
		sourceName := source.Name()
		targetName := newName
		if target != nil {
			targetName = target.Name()
		}
		first, second := source, target
		if second == source {
			second = nil
		} else if second != nil && second.inode < first.inode {
			first, second = second, first
		}
		first.muRW.Lock()
		if second != nil {
			second.muRW.Lock()
		}
		unlock := func() {
			if second != nil {
				second.muRW.Unlock()
			}
			first.muRW.Unlock()
		}
		d.mu.Lock()
		resolvedSource, sourceErr := d._renameBinding(oldName)
		if !source.mu.TryRLock() {
			d.waitFile(source)
			d.mu.Unlock()
			unlock()
			continue
		}
		validSource := resolvedSource == source && d.items[sourceName] == source && source.d == d && source.leaf == sourceName && !source.unlinked && !source.unlinking
		source.mu.RUnlock()
		d.mu.Unlock()
		destDir.mu.Lock()
		resolvedTarget, targetErr := destDir._renameBinding(newName)
		validTarget := resolvedTarget == dest && destDir.items[targetName] == dest
		if target != nil {
			if !target.mu.TryRLock() {
				destDir.waitFile(target)
				destDir.mu.Unlock()
				unlock()
				continue
			}
			validTarget = validTarget && target.d == destDir && target.leaf == targetName && !target.unlinked && !target.unlinking
			target.mu.RUnlock()
		}
		destDir.mu.Unlock()
		if sourceErr != nil || targetErr != nil {
			unlock()
			return nil, nil, nil, errors.Join(sourceErr, targetErr)
		}
		if validSource && validTarget {
			return source, target, unlock, nil
		}
		unlock()
	}
}

// replaceDestination fences callbacks and deferred opens on the old File.
// The caller holds both lifecycle locks and the cache path reservations.
func (f *File) replaceDestination(d *Dir, name string, target *File, action func(*File) (bool, error)) error {
	// A concurrent rename to an initially absent target has no shared File
	// lock. Verify again after the cache reservation excludes that rename.
	var expected Node
	if target != nil {
		expected = target
	}
	for {
		d.mu.Lock()
		resolved, bindingErr := d._renameBinding(name)
		if bindingErr != nil {
			d.mu.Unlock()
			return bindingErr
		}
		if resolved != expected {
			d.mu.Unlock()
			return errRenameBindingChanged
		}
		if target != nil {
			if !target.mu.TryLock() {
				d.waitFile(target)
				d.mu.Unlock()
				continue
			}
			target.unlinking = true
			target.mu.Unlock()
		}
		d.mu.Unlock()
		break
	}
	committed, err := action(target)
	if target != nil {
		var recoveryErr *vfscache.RenameRecoveryError
		if committed {
			// Replace the visible binding under one directory lock: readers
			// must never observe an absent destination between generations.
			for {
				d.mu.Lock()
				if !target.mu.TryLock() {
					d.waitFile(target)
					d.mu.Unlock()
					continue
				}
				if !f.mu.TryRLock() {
					target.mu.Unlock()
					d.waitFile(f)
					d.mu.Unlock()
					continue
				}
				break
			}
			target.unlinked, target.unlinking = true, false
			target.pendingRenameFun = nil
			d._addObjectLeaf(f, f.leaf)
			f.mu.RUnlock()
			target.mu.Unlock()
			d.mu.Unlock()
		} else if errors.As(err, &recoveryErr) {
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

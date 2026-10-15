// Separate remote refresh work from the directory's short namespace transactions.
package vfs

import (
	"errors"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/dirtree"
)

var errDirectoryObservationChanged = errors.New("directory changed while refreshing")

type directoryRefresh struct {
	done       chan struct{}
	started    time.Time
	err        error
	generation uint64
}

// refreshDirectory requires Dir.mu and returns with it held. A forced refresh
// retains the last successful timestamp until an accepted replacement exists.
func (d *Dir) refreshDirectory(force, strict bool) error {
	for {
		if flight := d.refresh; flight != nil {
			if !strict {
				return nil
			}
			d.mu.Unlock()
			select {
			case <-flight.done:
			case <-d.vfs.ctx.Done():
			}
			d.mu.Lock()
			if err := d.vfs.ctx.Err(); err != nil {
				return err
			}
			// Completion is not ownership: invalidation may have won Dir.mu
			// before this waiter, so the old observation cannot satisfy it.
			if errors.Is(flight.err, errDirectoryObservationChanged) || flight.generation != d.generation {
				continue
			}
			if _, stale := d._age(time.Now()); flight.err == nil && stale {
				continue
			}
			return flight.err
		}
		if !force {
			if _, stale := d._age(time.Now()); !stale {
				return nil
			}
		}
		flight := &directoryRefresh{done: make(chan struct{}), started: time.Now(), generation: d.generation}
		d.refresh = flight
		path, generation := d.path, d.generation
		err := func() (err error) {
			defer func() { flight.err = err; d.refresh = nil; close(flight.done) }()
			defer vfscommon.RecoverPanic(d.f, &err)
			var entries fs.DirEntries
			func() { d.mu.Unlock(); defer d.mu.Lock(); entries, err = d.readDirectory(path) }()
			if generation != d.generation || path != d.path {
				return errDirectoryObservationChanged
			}
			if err != nil {
				return err
			}
			err = d._readDirFromEntries(entries, nil, flight.started)
			if generation != d.generation || path != d.path {
				return errDirectoryObservationChanged
			}
			if err == nil {
				d.read = time.Now()
				d.cleanupTimer.Reset(time.Duration(d.vfs.Opt.DirCacheTime * 2))
			}
			return err
		}()
		if errors.Is(err, errDirectoryObservationChanged) {
			continue
		}
		return err
	}
}

// Only still-valid snapshots and locally newer mutations bypass an in-flight
// refresh. Expiration/invalidation and unknown names retain strict resolution.
func (d *Dir) cachedDuringRefresh(leaf string) bool {
	if d.refresh == nil || d.items[leaf] == nil {
		return false
	}
	if _, stale := d._age(time.Now()); !stale {
		return true
	}
	return d.refresh.generation == d.generation && d.namespaceUpdated[leaf].After(d.refresh.started)
}

func (d *Dir) noteNamespaceChange(leaf string) {
	if d.namespaceUpdated == nil {
		d.namespaceUpdated = make(map[string]time.Time)
	}
	d.namespaceUpdated[leaf] = time.Now()
}

func (d *Dir) newerNamespace(leaf string, when time.Time) bool {
	return !when.IsZero() && d.namespaceUpdated[leaf].After(when)
}

func (d *Dir) observationCurrent(generation uint64, when time.Time) bool {
	return d.generation == generation && (when.IsZero() || !when.Before(d.observedAfter))
}

// Wait outside Dir.mu, then let the caller recheck the current binding and
// observation. No File-to-Dir nested acquisition is introduced.
func (d *Dir) waitFile(file *File) {
	d.mu.Unlock()
	file.mu.Lock()
	file.mu.Unlock()
	d.mu.Lock()
}

func (d *Dir) waitChild(child *Dir) {
	d.mu.Unlock()
	child.mu.Lock()
	child.mu.Unlock()
	d.mu.Lock()
}

func (d *Dir) updateListedFile(leaf string, object fs.Object, when time.Time) Node {
	generation := d.generation
	for {
		node := d.items[leaf]
		if !d.observationCurrent(generation, when) || d.newerNamespace(leaf, when) {
			return node
		}
		file, ok := node.(*File)
		if !ok {
			return newFile(d, d.path, object, leaf)
		}
		if !file.mu.TryLock() {
			d.waitFile(file)
			continue
		}
		if when.IsZero() || !file.objectUpdated.After(when) {
			file.o = object
			file.objectRevision++
			file._setIsLink()
			file.virtualModTime = nil
		}
		file.mu.Unlock()
		return file
	}
}

type preparedDirectoryEntry struct {
	entry   fs.DirEntry
	name    string
	modTime time.Time
}

func (d *Dir) prepareEntries(entries fs.DirEntries) []preparedDirectoryEntry {
	d.mu.Unlock()
	defer d.mu.Lock()
	prepared := make([]preparedDirectoryEntry, 0, len(entries))
	for _, entry := range entries {
		row := preparedDirectoryEntry{entry: entry, name: entry.Remote()}
		if directory, ok := entry.(fs.Directory); ok {
			row.modTime = directory.ModTime(d.vfs.ctx)
		}
		prepared = append(prepared, row)
	}
	return prepared
}

func (d *Dir) updateListedDirectory(leaf string, entry fs.Directory, modTime time.Time, tree dirtree.DirTree, when time.Time, observations ...map[*Dir]uint64) (Node, error) {
	generation := d.generation
	for {
		node := d.items[leaf]
		if !d.observationCurrent(generation, when) {
			return nil, errDirectoryObservationChanged
		}
		if d.newerNamespace(leaf, when) {
			return node, nil
		}
		child, ok := node.(*Dir)
		if !ok {
			child = newDirObserved(d.vfs, d.f, d, entry, modTime)
		}
		if !child.mu.TryLock() {
			d.waitChild(child)
			continue
		}
		child.modTimeMu.Lock()
		child.modTime = modTime
		child.modTimeMu.Unlock()
		child.entry = entry
		if tree == nil {
			child.mu.Unlock()
			return child, nil
		}
		// Recursive reconciliation may wait on a descendant. No ancestor mutex
		// may remain held during that wait.
		var err error
		func() {
			d.mu.Unlock()
			defer d.mu.Lock()
			defer child.mu.Unlock()
			err = child._readDirFromDirTree(tree, when, observations...)
			if err == nil {
				child.read = when
				child.cleanupTimer.Reset(time.Duration(d.vfs.Opt.DirCacheTime * 2))
			} else {
				child.read = time.Time{}
			}
		}()
		if !d.observationCurrent(generation, when) {
			return nil, errDirectoryObservationChanged
		}
		return child, err
	}
}

// Snapshot each existing directory without retaining ancestor locks while
// descending. A changed root/path or child generation invalidates its reply.
func (d *Dir) refreshGenerations(out map[*Dir]uint64) {
	d.mu.RLock()
	out[d] = d.generation
	var children []*Dir
	for _, node := range d.items {
		if child, ok := node.(*Dir); ok {
			children = append(children, child)
		}
	}
	d.mu.RUnlock()
	for _, child := range children {
		child.refreshGenerations(out)
	}
}

// gclone-local object-view fencing for upload/listing races; see UPSTREAM.txt.
package vfs

import (
	"reflect"
	"time"

	"github.com/rclone/rclone/fs"
)

// setListingNode checks at the mutation itself, not at a pre-scan which an
// upload can overtake. Dir.mu is held; File.mu stays held through replacement
// or deletion so local publication cannot slip between the check and mutation.
func (d *Dir) setListingNode(name string, node Node, readStarted time.Time) {
	if f, ok := d.items[name].(*File); ok {
		f.mu.RLock()
		defer f.mu.RUnlock()
		if !readStarted.IsZero() && f.objectUpdated.After(readStarted) {
			return
		}
	}
	if node == nil {
		delete(d.items, name)
	} else {
		d.items[name] = node
	}
}

func (f *File) objectSnapshot() (fs.Object, uint64) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.o, f.objectRevision
}

// Reconcile only the snapshot whose cache open just succeeded. A concurrent
// listing or upload publication wins. Item locks are already released here:
// acquiring File.mu from inside Item.Open would invert File->Cache->Item.
func (f *File) reconcileObject(o fs.Object, revision uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unlinked || f.unlinking || f.objectRevision != revision {
		return
	}
	// Compare reference identity without requiring every fs.Object concrete
	// type to be Go-comparable. Most backends implement objects as pointers.
	if reflect.ValueOf(f.o) == reflect.ValueOf(o) {
		return
	}
	f.o = o
	f.objectRevision++
	f._setIsLink()
	f.virtualModTime = nil
	// This was a read, not a local publication. Advancing objectUpdated here
	// could suppress a legitimate external change in an in-flight listing.
}

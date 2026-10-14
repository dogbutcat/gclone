// gclone-local observation ownership for cache opens; see UPSTREAM.txt.
package vfscache

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscache/downloaders"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/ranges"
)

// An observation is valid only while the Item's name and ownership stay put.
// This state is intentionally not persisted in Info or copied to replacements.
type objectObservation struct {
	epoch       uint64
	writeEpoch  uint64
	name        string
	verified    bool
	sourceReady bool
}

func sameObjectReference(a, b fs.Object) bool {
	return reflect.ValueOf(a) == reflect.ValueOf(b)
}

func sameObjectIdentity(a, b fs.Object) bool {
	if sameObjectReference(a, b) {
		return true
	}
	left, lok := a.(fs.IDer)
	right, rok := b.(fs.IDer)
	return lok && rok && left.ID() != "" && left.ID() == right.ID()
}

// All helpers below require Item.mu unless noted otherwise.
func (item *Item) _objectObservation() objectObservation {
	return objectObservation{epoch: item.objectEpoch, writeEpoch: item.writeEpoch, name: item.name}
}

func (item *Item) _ownsObservation(obs objectObservation) bool {
	return item.objectEpoch == obs.epoch && item.writeEpoch == obs.writeEpoch && item.pendingWrites == 0 &&
		item.name == obs.name && !item.unlinked && !item.unlinking
}

func (item *Item) _beginWrite() {
	item.writeEpoch++
	item.pendingWrites++
}

func (item *Item) _endWrite() {
	item.pendingWrites--
	item.cond.Broadcast()
}

func (item *Item) _observeAbsence() {
	// Destination recreation cannot supply bytes that were never cached from
	// the deleted source. This obligation survives later destination recovery.
	if !item._present() && (item.o != nil || item.info.Fingerprint != "") {
		item._loseSource()
	}
	item.mustRevalidate = true
	item.objectEpoch++ // also advance on repeated absence: true -> true is an event
}

func (item *Item) _loseSource() {
	if !item.sourceMissing {
		item.sourceFingerprint = item.info.Fingerprint
		item.sourceMissing = true
		item.objectEpoch++
		item.downloadEpoch++
	}
}

// Keep new range requests from starting another downloader while the old one
// is drained. Background writes are rejected until source acceptance commits.
func (item *Item) _stopDownloaders(obs objectObservation) error {
	if item.sourceChanging {
		return errObjectViewChanged
	}
	item.downloadEpoch++
	if dls := item.downloaders; dls != nil {
		item.downloaders = nil
		func() {
			item.sourceChanging = true
			defer func() { item.sourceChanging = false; item.cond.Broadcast() }()
			unlockMutexForCall(&item.mu, func() { _ = dls.Close(nil) })
		}()
	}
	if !item._ownsObservation(obs) {
		return errObjectViewChanged
	}
	return nil
}

type downloadTarget struct {
	*Item
	epoch uint64
}

func (target *downloadTarget) WriteAtNoOverwrite(b []byte, off int64) (int, int, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.epoch != target.downloadEpoch || target.unlinked || target.unlinking {
		return 0, 0, errObjectViewChanged
	}
	return target._writeAtNoOverwrite(b, off)
}

func (item *Item) _newDownloaders() *downloaders.Downloaders {
	item.downloadEpoch++
	target := &downloadTarget{Item: item, epoch: item.downloadEpoch}
	return downloaders.New(item.c.ctx, target, item.c.opt, item.name, item.o)
}

func (item *Item) _acceptObject(o fs.Object) {
	if o != nil {
		item.localCreation = false
	}
	if !sameObjectReference(item.o, o) {
		item.objectEpoch++
		item.o = o
	}
}

// A surviving source can supply its original prefix. An unrecorded local tail
// beyond that source keeps upstream's crash-recovery semantics. Once the source
// is absent/changed, every logical byte must be locally present before upload.
func (item *Item) _hasWritebackData() bool {
	size := item.info.Size
	if item.o != nil && !item.sourceMissing {
		if remoteSize := item.o.Size(); remoteSize >= 0 && remoteSize < size {
			size = remoteSize
		}
	}
	return size >= 0 && item.info.Rs.Present(ranges.Range{Pos: 0, Size: size})
}

// Invalidation must validate its owner BEFORE deleting bytes. _remove cannot
// supply that contract because it releases Item.mu while draining writeback.
func (item *Item) _invalidateObject(reason string, obs objectObservation, size int64) error {
	id := item.writeBackID
	unlockMutexForCall(&item.mu, func() { item.c.writeback.Remove(id) })
	if !item._ownsObservation(obs) || item.info.Dirty || item._kernelWriter() {
		return errObjectViewChanged
	}
	if err := item._stopDownloaders(obs); err != nil {
		return err
	}
	if item.info.Dirty || item._kernelWriter() {
		return errObjectViewChanged
	}
	// Open descriptors share this Item. Keep its named cache file attached:
	// unlinking here leaves subsequent writes on an anonymous fd that _store
	// cannot upload. Item.mu serializes physical reads/writes, and downloaders
	// have drained above. Removing persisted ranges first makes interruption
	// recover as a clean cache miss, never as valid bytes from the old source.
	if item.fd != nil {
		if err := os.Remove(item.c.toOSPathMeta(item.name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("vfs cache: invalidate metadata: %w", err)
		}
		if size >= 0 {
			if err := item.fd.Truncate(size); err != nil {
				return fmt.Errorf("vfs cache: resize replacement: %w", err)
			}
		}
	} else {
		item._removeFile(reason)
		item._removeMeta(reason)
	}
	item.objectEpoch++
	item._kernelReplacement()
	item.info.clean()
	item.info.Size = size
	return nil
}

func (item *Item) _checkObjectView(o fs.Object, allowCreate bool) (objectObservation, error) {
	obs := item._objectObservation()
	fingerprint := ""
	if o != nil {
		fingerprint = fs.Fingerprint(item.c.ctx, o, item.c.opt.FastFingerprint)
	}
	identityConflict := o != nil && ((item.o != nil && !sameObjectIdentity(item.o, o)) || item.localCreation)
	lookup := item.mustRevalidate || item.publishing || identityConflict ||
		(!item.info.Dirty && item.info.Fingerprint != "" && fingerprint != item.info.Fingerprint)
	if lookup {
		fresh, err := item._lookupObject(item.c.ctx)
		switch {
		case errors.Is(err, fs.ErrorObjectNotFound):
			item._observeAbsence()
			if (!item.info.Dirty || item.publishing) && !allowCreate {
				return objectObservation{}, os.ErrNotExist
			}
			o = nil // retain dirty bytes, but never write them to a deleted ID
		case err != nil:
			return objectObservation{}, fmt.Errorf("vfs cache: revalidate conflicting object: %w", err)
		case fresh == nil:
			return objectObservation{}, errors.New("vfs cache: revalidate returned nil object without an error")
		default:
			o = fresh
			obs.verified = true
		}
	} else if item.o != nil && fingerprint == item.info.Fingerprint {
		// Content equivalence never transfers ownership to an older snapshot.
		o = item.o
	}
	obs.epoch, obs.name = item.objectEpoch, item.name

	if o == nil {
		if item.info.Fingerprint != "" && !item.info.Dirty {
			if !allowCreate || !item._kernelWriter() {
				if err := item._invalidateObject("stale (remote deleted)", obs, 0); err != nil {
					return objectObservation{}, err
				}
			}
		}
	} else {
		remoteFingerprint := fs.Fingerprint(item.c.ctx, o, item.c.opt.FastFingerprint)
		// A writable kernel descriptor can contain dirty mmap pages which have
		// not reached WriteAt yet. Retain its accepted data until that writable
		// lifetime closes; fresh opens will revalidate the deferred observation.
		if !item.info.Dirty && item.info.Fingerprint != "" &&
			remoteFingerprint != item.info.Fingerprint && item._kernelWriter() {
			item.mustRevalidate = true
			if !item._present() {
				item._loseSource()
			}
			return item._objectObservation(), nil
		}
		if obs.verified && item.info.Dirty && !item._present() && item.info.Fingerprint != "" {
			// Fresh metadata matching the original source permits refetching
			// missing ranges. A different destination cannot supply those bytes.
			if item.sourceMissing {
				if item.sourceFingerprint != "" && remoteFingerprint == item.sourceFingerprint {
					obs.sourceReady = true
				}
			} else if remoteFingerprint != item.info.Fingerprint {
				item._loseSource()
			}
		}
		if obs.verified && !item.info.Dirty {
			obs.sourceReady = true // clean replacement has no local edits to retain
		}
		if item.sourceMissing && obs.sourceReady {
			if err := item._stopDownloaders(obs); err != nil {
				return objectObservation{}, err
			}
		}
		if remoteFingerprint != item.info.Fingerprint && !item.info.Dirty &&
			(item.info.Fingerprint != "" || item._exists()) {
			if err := item._invalidateObject("stale (remote is different)", obs, o.Size()); err != nil {
				return objectObservation{}, err
			}
		}
		if item.info.Fingerprint == "" {
			item.info.Fingerprint = remoteFingerprint
		}
		item.info.Size = o.Size()
	}
	item._acceptObject(o)
	if err := item._truncateToCurrentSize(); err != nil {
		return objectObservation{}, fmt.Errorf("vfs cache item: open truncate failed: %w", err)
	}
	obs.epoch, obs.name = item.objectEpoch, item.name
	return obs, nil
}

// Creation is committed only after the cache descriptor exists and earlier
// writeback has drained. Maintenance opens and Reset never call this method.
func (item *Item) _commitCreate(obs objectObservation) error {
	id := item.writeBackID
	unlockMutexForCall(&item.mu, func() { item.c.writeback.Remove(id) })
	if !item._ownsObservation(obs) || item.o != nil || item.fd == nil {
		return errObjectViewChanged
	}
	if err := item._stopDownloaders(obs); err != nil {
		return err
	}
	dirty, modified, modTime, accessTime := item.info.Dirty, item.modified, item.info.ModTime, item.info.ATime
	item.info.Dirty, item.modified = true, true
	item.info.ModTime = time.Now()
	item.info.ATime = item.info.ModTime
	if err := item._save(); err != nil {
		item.info.Dirty, item.modified = dirty, modified
		item.info.ModTime, item.info.ATime = modTime, accessTime
		return err
	}
	item.objectEpoch++
	item.localCreation = true
	if item._present() {
		item.sourceMissing = false
		item.sourceFingerprint = ""
	}
	item.mustRevalidate = false
	return nil
}

// Drop only the open acquired by a failed ownership commit. Do not queue
// writeback or publish a File for an open which never succeeded.
func (item *Item) _abortObjectOpen() {
	item.opens--
	if item.opens != 0 {
		return
	}
	item._closeUnusedHandle()
}

// Close only an unused descriptor, retaining dirty data/ranges for recovery.
func (item *Item) _closeUnusedHandle() {
	if item.graceTimer != nil {
		item.graceTimer.Stop()
		item.graceTimer = nil
	}
	if item.closing == nil {
		item.closing = make(chan struct{})
		defer func() { close(item.closing); item.closing = nil }()
	}
	if dls := item.downloaders; dls != nil {
		item.downloaders = nil
		unlockMutexForCall(&item.mu, func() { _ = dls.Close(nil) })
	}
	if item.fd != nil {
		_ = item.fd.Close()
		item.fd = nil
	}
}

// Track accepted kernel data views independently from upload and path metadata.
package vfscache

import (
	"context"
	"os"
	"sync"

	"github.com/rclone/rclone/fs"
)

// KernelCacheOwner identifies one canonical kernel inode view of an Item.
// The nonzero size keeps distinct owner allocations distinct in Go.
type KernelCacheOwner struct {
	mu         sync.Mutex
	item       *Item
	invalidate func()
	wait       func(context.Context) error
}

// NewKernelCacheOwner allocates the identity retained by a mount's inode wrapper.
func NewKernelCacheOwner() *KernelCacheOwner { return new(KernelCacheOwner) }

// NewKernelCacheOwnerWithInvalidation keeps notifications alive for the kernel
// node, including pages retained after its last descriptor closes. Callbacks
// are immutable. invalidate only queues work; wait is called outside Item.mu.
func NewKernelCacheOwnerWithInvalidation(invalidate func(), wait func(context.Context) error) *KernelCacheOwner {
	return &KernelCacheOwner{invalidate: invalidate, wait: wait}
}

func (owner *KernelCacheOwner) changed() {
	if owner.invalidate != nil {
		owner.invalidate()
	}
}

func (owner *KernelCacheOwner) bind(item *Item) {
	owner.mu.Lock()
	changed := owner.item != nil && owner.item != item
	owner.item = item
	owner.mu.Unlock()
	if changed {
		owner.changed()
	}
}

// SyncKernelViews waits only for queued notifications, never while holding
// Item.mu or the lock used by physical cache reads and writes.
func (item *Item) SyncKernelViews(ctx context.Context) error {
	item.mu.Lock()
	owners := make([]*KernelCacheOwner, 0, len(item.kernelViews))
	for owner := range item.kernelViews {
		owners = append(owners, owner)
	}
	item.mu.Unlock()
	for _, owner := range owners {
		if owner.wait != nil {
			if err := owner.wait(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

type kernelViewState struct {
	refs, writers int
	foreignWrites uint64
}

// KernelCacheVersion is an opaque comparable identity for cached file contents.
// Local writes through this owner and upload publication do not replace its view.
type KernelCacheVersion struct {
	item                *Item
	owner               *KernelCacheOwner
	data, foreignWrites uint64
}

// KernelCacheLease pins a prepared descriptor's cache view until its close.
type KernelCacheLease struct {
	item               *Item
	owner              *KernelCacheOwner
	writable, released bool
}

// Version returns the current accepted contents, without rediscovering a pathname.
func (lease *KernelCacheLease) Version() (KernelCacheVersion, error) {
	item := lease.item
	item.mu.Lock()
	defer item.mu.Unlock()
	if lease.released {
		return KernelCacheVersion{}, os.ErrClosed
	}
	state := item.kernelViews[lease.owner]
	return KernelCacheVersion{item, lease.owner, item.kernelDataEpoch, state.foreignWrites}, nil
}

// Close releases the pin after the corresponding Item.Close completes.
func (lease *KernelCacheLease) Close() {
	if lease == nil {
		return
	}
	item := lease.item
	item.mu.Lock()
	defer item.mu.Unlock()
	if lease.released {
		return
	}
	lease.released = true
	state := item.kernelViews[lease.owner]
	state.refs--
	item.kernelPins--
	if lease.writable {
		state.writers--
	}
	item.cond.Broadcast()
}

// The following helpers require Item.mu.
func (item *Item) _kernelWriter() bool {
	for _, state := range item.kernelViews {
		if state.writers != 0 {
			return true
		}
	}
	return false
}

func (item *Item) _foreignKernelWriter(owner *KernelCacheOwner) bool {
	for key, state := range item.kernelViews {
		if key != owner && state.writers != 0 {
			return true
		}
	}
	return false
}

func (item *Item) _kernelMutation(owner *KernelCacheOwner) {
	for key, state := range item.kernelViews {
		if key != owner {
			state.foreignWrites++
			key.changed()
		}
	}
}

func (item *Item) _kernelReplacement() {
	item.kernelDataEpoch++
	for owner := range item.kernelViews {
		owner.changed()
	}
}

func (item *Item) _bindKernelView(owner *KernelCacheOwner, writable bool) *KernelCacheLease {
	if item.kernelViews == nil {
		item.kernelViews = make(map[*KernelCacheOwner]*kernelViewState)
	}
	state := item.kernelViews[owner]
	if state == nil {
		state = new(kernelViewState)
		item.kernelViews[owner] = state
	}
	state.refs++
	item.kernelPins++
	if writable {
		state.writers++
	}
	owner.bind(item)
	return &KernelCacheLease{item: item, owner: owner, writable: writable}
}

// OpenForKernel combines source acceptance and ownership registration atomically.
func (item *Item) OpenForKernel(o fs.Object, allowCreate bool, owner *KernelCacheOwner, writable bool) (fs.Object, *KernelCacheLease, error) {
	request := &kernelOpen{owner: owner, writable: writable}
	resolved, err := item.openObjectForKernel(o, allowCreate, allowCreate, request)
	return resolved, request.lease, err
}

type kernelOpen struct {
	owner    *KernelCacheOwner
	writable bool
	lease    *KernelCacheLease
}

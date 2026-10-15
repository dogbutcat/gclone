// Keep cache namespace transactions independent from per-file I/O and maintenance.
package vfscache

import (
	"errors"
	"github.com/rclone/rclone/fs"
	"os"
	"time"
)

// ErrStaleItem means a captured cache pointer no longer owns its pathname.
// VFS may reacquire the current Item while retaining its File lifecycle lock.
var ErrStaleItem = errors.New("cache item binding changed before open")

// admitOpen runs after preAccess pins the captured Item, but before any
// pathname I/O. A stale pointer must never create files and only later find
// out that another dirty lifetime owns the same name.
func (c *Cache) admitOpen(item *Item) error {
	for {
		name := item.GetName()
		c.lockNames(name)
		if !c.tryItems(item) {
			continue
		}
		if item.name != name {
			item.mu.Unlock()
			c.mu.Unlock()
			continue
		}
		var err error
		if item.unlinkErr != nil {
			err = item.unlinkErr
		} else if item.unlinked || item.unlinking {
			err = os.ErrNotExist
		} else if current := c.item[name]; current != nil && current != item {
			err = ErrStaleItem
		} else if current == nil {
			c.bindItem(name, item)
		}
		item.mu.Unlock()
		c.mu.Unlock()
		return err
	}
}

type cacheActivity struct {
	done chan struct{}
}

// TryInUse is for callers already holding a namespace lock. A busy result
// must be retried after releasing that lock; it never means absent or clean.
func (c *Cache) TryInUse(name string) (inUse, ready bool) {
	name = clean(name)
	if !c.mu.TryLock() {
		return false, false
	}
	defer c.mu.Unlock()
	if c.activities[name] != nil {
		return false, false
	}
	item := c.item[name]
	if item == nil {
		return false, true
	}
	if !item.mu.TryLock() {
		return false, false
	}
	defer item.mu.Unlock()
	return item.opens != 0 || item.info.Dirty || item.pendingLookups != 0 || item.unlinking || item.unlinked, true
}

type cacheBinding struct {
	name  string
	item  *Item
	epoch uint64
}

// lockNames returns with Cache.mu held and no initialization/maintenance for
// these names. Explicit rename reservations are deliberately not waited on:
// their owners must still be able to resolve and retire their own cache items.
func (c *Cache) lockNames(names ...string) {
	for {
		c.mu.Lock()
		var done <-chan struct{}
		for _, name := range names {
			if activity := c.activities[name]; activity != nil {
				done = activity.done
				break
			}
		}
		if done == nil {
			return
		}
		c.mu.Unlock()
		<-done
	}
}

func waitForItem(item *Item) { item.mu.Lock(); item.mu.Unlock() }

// tryItems is called with Cache.mu held. A failed acquisition releases all
// acquired Item locks and Cache.mu before waiting; the caller must retry its
// entire namespace observation. Success retains Cache.mu and all Item locks.
func (c *Cache) tryItems(items ...*Item) bool {
	locked := make([]*Item, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		duplicate := false
		for _, prior := range locked {
			if prior == item {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		if !item.mu.TryLock() {
			for _, prior := range locked {
				prior.mu.Unlock()
			}
			c.mu.Unlock()
			waitForItem(item)
			return false
		}
		locked = append(locked, item)
	}
	return true
}

func (c *Cache) bindItem(name string, item *Item) {
	if c.epochs == nil {
		c.epochs = make(map[string]uint64)
	}
	c.bindingEpoch++
	c.epochs[name] = c.bindingEpoch
	if item == nil {
		delete(c.item, name)
		delete(c.epochs, name)
	} else {
		c.item[name] = item
	}
}

func (c *Cache) bindings() []cacheBinding {
	c.mu.Lock()
	defer c.mu.Unlock()
	bindings := make([]cacheBinding, 0, len(c.item))
	for name, item := range c.item {
		bindings = append(bindings, cacheBinding{name, item, c.epochs[name]})
	}
	return bindings
}

func (c *Cache) ownsBinding(binding cacheBinding) bool {
	return c.item[binding.name] == binding.item && c.epochs[binding.name] == binding.epoch
}

// lockItem observes the current binding jointly with Item state, without
// waiting for its mutex while retaining the cache-wide mutex.
func (c *Cache) lockItem(name string) *Item {
	for {
		item, _ := c.get(name)
		c.lockNames(name)
		if c.item[name] != item {
			c.mu.Unlock()
			continue
		}
		if c.tryItems(item) {
			return item
		}
	}
}

type cacheMaintenance struct {
	c        *Cache
	binding  cacheBinding
	activity *cacheActivity
}

// claimMaintenance also fences callers which captured Item before this claim:
// preAccess waits for maintenance, and an already active access prevents claim.
func (c *Cache) claimMaintenance(binding cacheBinding) *cacheMaintenance {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ownsBinding(binding) || c.activities[binding.name] != nil || c.mutations[binding.name] != nil {
		return nil
	}
	item := binding.item
	if !item.mu.TryLock() {
		return nil
	}
	defer item.mu.Unlock()
	if item.pendingAccesses != 0 || item.pendingLookups != 0 || item.pendingWrites != 0 || item.closing != nil ||
		item.info.Dirty || item.publishing || item.unlinking || item.unlinked || item.maintenance != nil || item._kernelWriter() {
		return nil
	}
	activity := &cacheActivity{done: make(chan struct{})}
	if c.activities == nil {
		c.activities = make(map[string]*cacheActivity)
	}
	c.activities[binding.name] = activity
	item.maintenance = activity
	return &cacheMaintenance{c, binding, activity}
}

func (m *cacheMaintenance) finish(removed bool, resetErr error) {
	c, item := m.c, m.binding.item
	for {
		c.mu.Lock()
		if c.tryItems(item) {
			break
		}
	}
	defer c.mu.Unlock()
	defer item.mu.Unlock()
	if c.ownsBinding(m.binding) {
		if removed {
			if item.maintenanceWaiters != 0 {
				c.bindItem(m.binding.name, item)
			} else {
				c.bindItem(m.binding.name, nil)
			}
			delete(c.errItems, m.binding.name)
			delete(c.errOwners, m.binding.name)
		} else if resetErr != nil {
			c.errItems[m.binding.name] = resetErr
			if c.errOwners == nil {
				c.errOwners = make(map[string]cacheBinding)
			}
			c.errOwners[m.binding.name] = m.binding
		} else {
			delete(c.errItems, m.binding.name)
			delete(c.errOwners, m.binding.name)
		}
	}
	item.maintenance = nil
	item.cond.Broadcast()
	delete(c.activities, m.binding.name)
	close(m.activity.done)
}

type maintenanceCandidate struct {
	cacheBinding
	access time.Time
}

func (c *Cache) maintenanceCandidates() []maintenanceCandidate {
	var candidates []maintenanceCandidate
	for _, binding := range c.bindings() {
		item := binding.item
		if !item.mu.TryLock() {
			continue
		}
		candidates = append(candidates, maintenanceCandidate{binding, item.info.ATime})
		item.mu.Unlock()
	}
	return candidates
}

// Quota sampling can involve statfs and is always outside the cache mutex.
func (c *Cache) quotaForSize(used int64) bool {
	return (c.opt.CacheMaxSize <= 0 || used <= int64(c.opt.CacheMaxSize)) && c.minFreeSpaceQuotaOK()
}

func (c *Cache) evictBinding(binding cacheBinding, maxAge time.Duration, emptyOnly bool) int64 {
	owner := c.claimMaintenance(binding)
	if owner == nil {
		return 0
	}
	removed := false
	defer func() { owner.finish(removed, nil) }()
	removed, freed := binding.item.RemoveNotInUse(maxAge, emptyOnly)
	return freed
}

func (c *Cache) resetBinding(binding cacheBinding) int64 {
	owner := c.claimMaintenance(binding)
	if owner == nil {
		return 0
	}
	result := SkippedPendingAccess
	var err error
	defer func() { owner.finish(result == RemovedNotInUse, err) }()
	result, freed, err := binding.item.Reset()
	if err != nil {
		fs.Errorf(binding.name, "vfs cache: reset failed: %v", err)
	}
	return freed
}

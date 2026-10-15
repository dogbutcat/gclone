// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

// Separate explicit unlink from reusable cache eviction and reset.
package vfscache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
)

// Unlink drains writeback, removes the latest remote object through remove,
// and retires this cache lifetime. The caller must serialize file open/close
// against this operation. A nil object lets the caller use its own object.
// removed reports whether remote deletion committed, even if local cleanup
// failed. On remote failure, cached data and pending writeback are retained.
//
// gclone hotfix for upstream v1.75.1: do not reuse Remove's eviction semantics
// for unlink. An old descriptor must never publish or touch a replacement's
// pathname. Drop this patch only after upstream passes the unlink regressions.
// This package is copied into gclone. See vfs/UPSTREAM.txt and UPSTREAM.json
// for provenance, the command adapters, and removal after an upstream fix.
//
// Scope: already cached bytes remain usable through open descriptors. This
// does not eagerly download sparse files before unlink; an uncached range or
// an unmaterialized old handle fails instead of reopening a replacement.
// This is not a claim of SQLite durability or a fix for every SIGBUS cause.
// Quarantined files left after process termination or rollback/cleanup errors
// stay outside both reload roots and require manual recovery or cleanup.
func (c *Cache) Unlink(name string, remove func(fs.Object) error) (removed bool, err error) {
	name = clean(name)
	mutation, err := c.reservePaths(name)
	if err != nil {
		return false, err
	}
	defer mutation.Close()
	return c.retire(name, func(o fs.Object, _ string) (bool, error) {
		err := remove(o)
		return err == nil, err
	})
}

// retire stages a destination lifetime while its caller owns the pathname
// reservation. The action may replace that pathname rather than delete it.
// Only a pre-commit error permits restoring the old data. A post-commit error
// keeps recovery files outside reload roots and still fences the old handles.
func (c *Cache) retire(name string, action func(fs.Object, string) (bool, error)) (committed bool, err error) {
	item := c.lockItem(name)
	if item.unlinking || item.unlinked || item.name != name {
		item.mu.Unlock()
		c.mu.Unlock()
		return false, os.ErrNotExist
	}
	item.unlinking = true
	id := item.writeBackID
	item.mu.Unlock()
	c.mu.Unlock()

	// Never hold cache/item locks while draining a callback. A backend may
	// finish successfully despite cancellation; _store retains that object.
	c.writeback.Remove(id)
	item.mu.Lock()
	for item.pendingAccesses != 0 || item.closing != nil {
		if item.closing != nil {
			closing := item.closing
			item.mu.Unlock()
			<-closing
			item.mu.Lock()
		} else {
			item.cond.Wait()
		}
	}
	dls := item.downloaders
	item.downloaders = nil
	item.mu.Unlock()
	if dls != nil {
		if closeErr := dls.Close(nil); closeErr != nil {
			fs.Errorf(name, "vfs cache: stopping downloader before unlink: %v", closeErr)
		}
	}

	// Quarantine both files outside the data and metadata scan roots before
	// deleting remotely. A local cleanup failure (or a restart) must not let
	// reload treat deleted data as a dirty file and upload it again.
	stageRoot := filepath.Join(config.GetCacheDir(), "vfs-unlinked")
	err = os.MkdirAll(stageRoot, 0700)
	var stage string
	if err == nil {
		stage, err = os.MkdirTemp(stageRoot, "unlink-")
	}
	paths := []string{c.toOSPath(name), c.toOSPathMeta(name)}
	staged := make([]bool, len(paths))
	stagePath := func(i int) string { return filepath.Join(stage, fmt.Sprint(i)) }
	if err == nil {
		for i, path := range paths {
			err = os.Rename(path, stagePath(i))
			if os.IsNotExist(err) {
				err = nil
				continue
			}
			if err != nil {
				break
			}
			staged[i] = true
		}
	}
	item.mu.Lock()
	o := item.o
	item.mu.Unlock()
	if err == nil {
		committed, err = action(o, stage)
	}
	if !committed {
		// Restore bytes first, then persist any writes made during deletion.
		// On a filesystem failure keep the reservation and recovery files;
		// never resume an upload through an incompletely restored pathname.
		for i, moved := range staged {
			if moved {
				if restoreErr := os.Rename(stagePath(i), paths[i]); restoreErr != nil {
					item.mu.Lock()
					item.unlinkErr = errors.Join(err, fmt.Errorf("vfs cache: restore from %q: %w", stage, restoreErr))
					item.unlinked, item.unlinking = true, false
					item.beingReset = false
					item.cond.Broadcast()
					item.mu.Unlock()
					return false, item.unlinkErr
				}
			}
		}
		if stage != "" {
			_ = os.Remove(stage)
		}
		item.mu.Lock()
		item.unlinking = false
		if item._exists() {
			if saveErr := item._save(); saveErr != nil {
				item.unlinkErr = errors.Join(err, saveErr)
				item.unlinked = true
				item.beingReset = false
				item.cond.Broadcast()
				item.mu.Unlock()
				return false, item.unlinkErr
			}
		}
		resume := item.info.Dirty && item.opens == 0 && item.fd == nil
		size, modified, storeFn := item.info.Size, item.modified, item.storeFn
		if resume {
			c.writeback.SetID(&item.writeBackID)
			id = item.writeBackID
		}
		item.cond.Broadcast()
		item.mu.Unlock()
		if resume {
			c.writeback.Add(id, name, size, modified, func(ctx context.Context) error {
				return item.store(ctx, storeFn)
			})
		}
		return false, err
	}

	// Keep name reserved until both pathnames are gone. Do not clear range
	// metadata or close the FD: late writes still belong to this old inode.
	for {
		c.mu.Lock()
		if c.tryItems(item) {
			break
		}
	}
	item.unlinked, item.unlinking = true, false
	if err != nil {
		item.unlinkErr = errors.Join(item.unlinkErr, err)
	}
	item.beingReset = false
	item.cond.Broadcast()
	if c.item[name] == item {
		c.bindItem(name, nil)
		delete(c.errItems, name)
		delete(c.errOwners, name)
	}
	item.mu.Unlock()
	c.mu.Unlock()
	// The outer pathname reservation still owns the unique quarantine paths.
	// Physical cleanup must not retain either shared cache or Item mutex.
	for i, moved := range staged {
		if moved && err == nil {
			if cleanupErr := os.Remove(stagePath(i)); cleanupErr != nil && !os.IsNotExist(cleanupErr) {
				fs.Errorf(name, "vfs cache: unlink committed; quarantined file %q needs cleanup: %v", stagePath(i), cleanupErr)
			}
		}
	}
	if err == nil {
		_ = os.Remove(stage)
	} else {
		err = fmt.Errorf("vfs cache: mutation committed; recovery data in %q: %w", stage, err)
	}
	return true, err
}

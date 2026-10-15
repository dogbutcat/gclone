// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

// Reserve individual cache pathnames across remote and local mutations.
package vfscache

import (
	"errors"
	"fmt"
	"sync"
	"syscall"

	"github.com/rclone/rclone/fs"
)

type pathMutation struct {
	c     *Cache
	names []string
	once  sync.Once
	done  chan struct{}
}

// PathBusyError identifies temporary namespace contention, not recovery failure.
// VFS callers can release their file locks, await Done, then resolve the paths
// again. Never wait on this channel while holding File/Item/cache locks.
type PathBusyError struct {
	name string
	done <-chan struct{}
}

func (e *PathBusyError) Error() string {
	return fmt.Sprintf("vfs cache: pathname %q is being changed", e.name)
}

func (e *PathBusyError) Unwrap() error { return syscall.EBUSY }

// Done closes after the conflicting mutation releases its pathnames.
func (e *PathBusyError) Done() <-chan struct{} { return e.done }

// reservePaths fails immediately on overlap rather than waiting with VFS
// locks held. Unrelated filenames can still be deleted/renamed concurrently.
func (c *Cache) reservePaths(names ...string) (*pathMutation, error) {
	for i, name := range names {
		names[i] = clean(name)
	}
	for {
		c.lockNames(names...)
		items := make([]*Item, 0, len(names))
		for _, name := range names {
			if owner := c.mutations[name]; owner != nil {
				c.mu.Unlock()
				return nil, &PathBusyError{name: name, done: owner.done}
			}
			item := c.item[name]
			if item == nil {
				continue
			}
			seen := false
			for _, prior := range items {
				if prior == item {
					seen = true
					break
				}
			}
			if !seen {
				items = append(items, item)
			}
		}
		if !c.tryItems(items...) {
			continue
		}
		var retired string
		for _, item := range items {
			if item.unlinking || item.unlinked {
				retired = item.name
			}
			item.mu.Unlock()
		}
		if retired != "" {
			c.mu.Unlock()
			return nil, fmt.Errorf("vfs cache: pathname %q needs recovery: %w", retired, syscall.EBUSY)
		}
		break
	}
	defer c.mu.Unlock()
	if c.mutations == nil {
		c.mutations = make(map[string]*pathMutation)
	}
	m := &pathMutation{c: c, names: names, done: make(chan struct{})}
	for _, name := range names {
		c.mutations[name] = m
	}
	return m, nil
}

func (m *pathMutation) Close() {
	m.once.Do(func() {
		m.c.mu.Lock()
		defer m.c.mu.Unlock()
		for _, name := range m.names {
			if m.c.mutations[name] == m {
				delete(m.c.mutations, name)
			}
		}
		close(m.done)
	})
}

// RenameMutation owns both pathnames until Close. Keep it across the remote
// move and Rename, but never across a deferred rename's writer lifetime.
type RenameMutation struct {
	*pathMutation
}

// BeginRename excludes unlink and another rename before any remote mutation.
func (c *Cache) BeginRename(name, newName string) (*RenameMutation, error) {
	m, err := c.reservePaths(name, newName)
	if err != nil {
		return nil, err
	}
	return &RenameMutation{pathMutation: m}, nil
}

// Rename moves the cache entries while this caller owns both pathnames.
func (m *RenameMutation) Rename(newObj fs.Object) error {
	_, _, err := m.apply(func(fs.Object) (fs.Object, error) { return newObj, nil }, false, m.c.rename)
	return err
}

// Apply retires the overwritten destination across the remote move and cache
// rename. A committed result must also advance the VFS namespace, even if a
// subsequent local failure needs recovery. Drop with the rename regressions
// once upstream owns this lifetime boundary; see vfs/UPSTREAM.txt.
func (m *RenameMutation) Apply(move func(fs.Object) (fs.Object, error)) (committed bool, newObj fs.Object, err error) {
	return m.apply(move, true, m.c.rename)
}

func (m *RenameMutation) apply(move func(fs.Object) (fs.Object, error), remote bool, renameSource func(string, string, fs.Object) error) (committed bool, newObj fs.Object, err error) {
	oldName, newName := m.names[0], m.names[1]
	if oldName == newName {
		return true, nil, nil
	}
	namespaceCommitted := false
	unrestored := false
	_, err = m.c.retire(newName, func(overwritten fs.Object, stage string) (bool, error) {
		var moveErr error
		newObj, moveErr = move(overwritten)
		if moveErr != nil {
			return false, moveErr
		}
		namespaceCommitted = remote && newObj != nil
		if renameErr := renameSource(oldName, newName, newObj); renameErr != nil {
			var rollbackErr *renameRollbackError
			if namespaceCommitted || errors.As(renameErr, &rollbackErr) {
				// A committed remote move or an unsafe local rollback cannot
				// restore the old target over source data. Preserve both in the
				// already-created retirement stage and fail old source handles.
				recoveryErr := m.c.quarantineRenameSource(oldName, newName, stage, renameErr)
				if !namespaceCommitted && recoveryErr == nil {
					// Source bytes are now safe outside both cache roots. The old
					// target can be restored without overwriting those bytes.
					return false, fmt.Errorf("source cache needs recovery in %q: %w", stage, renameErr)
				}
				unrestored = !namespaceCommitted
				return true, errors.Join(renameErr, recoveryErr)
			}
			return false, renameErr
		}
		namespaceCommitted = true
		return true, nil
	})
	committed = namespaceCommitted
	if unrestored {
		err = &RenameRecoveryError{err}
	}

	return committed, newObj, err
}

// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

// Reserve individual cache pathnames across remote and local mutations.
package vfscache

import (
	"fmt"
	"sync"
	"syscall"

	"github.com/rclone/rclone/fs"
)

type pathMutation struct {
	c     *Cache
	names []string
	once  sync.Once
}

// reservePaths fails immediately on overlap rather than waiting with VFS
// locks held. Unrelated filenames can still be deleted/renamed concurrently.
func (c *Cache) reservePaths(names ...string) (*pathMutation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, name := range names {
		names[i] = clean(name)
		if c.mutations[names[i]] != nil {
			return nil, fmt.Errorf("vfs cache: pathname %q is being changed: %w", names[i], syscall.EBUSY)
		}
		if item := c.item[names[i]]; item != nil {
			item.mu.Lock()
			retired := item.unlinking || item.unlinked
			item.mu.Unlock()
			if retired {
				return nil, fmt.Errorf("vfs cache: pathname %q needs recovery: %w", names[i], syscall.EBUSY)
			}
		}
	}
	if c.mutations == nil {
		c.mutations = make(map[string]*pathMutation)
	}
	m := &pathMutation{c: c, names: names}
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
	return m.c.rename(m.names[0], m.names[1], newObj)
}

// Keep known root-relative directory IDs independent from slow cache misses.
package drive

import (
	"context"

	"github.com/rclone/rclone/lib/dircache"
)

func (f *Fs) findDirectory(ctx context.Context, name string, create bool) (string, error) {
	// Upstream serializes FindDir, including cache hits, across FindLeaf and
	// CreateDir HTTP calls. Root-relative caches never undergo a root rebase,
	// so a confirmed ID can use Get's own synchronization without waiting for
	// an unrelated network miss. Nested roots retain upstream initialization
	// and reset semantics: their provisional entries use a different base.
	if f.root == "" {
		if id, ok := f.dirCache.Get(name); ok {
			return id, nil
		}
	}
	return f.dirCache.FindDir(ctx, name, create)
}

func (f *Fs) findParentDirectory(ctx context.Context, remote string, create bool) (leaf, id string, err error) {
	// Empty paths ask for the root's parent and retain DirCache's special case.
	if remote == "" {
		return f.dirCache.FindPath(ctx, remote, create)
	}
	parent, leaf := dircache.SplitPath(remote)
	id, err = f.findDirectory(ctx, parent, create)
	return leaf, id, err
}

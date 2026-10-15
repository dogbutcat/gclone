// Keep known root-relative directory IDs independent from slow cache misses.
package drive

import (
	"context"
	"errors"

	"github.com/dogbutcat/gclone/internal/dircache"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
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

// DirectoryCacheKey identifies the actual remote parent and encoded name.
// It is pure bookkeeping and must never issue requests under the cache lock.
func (f *Fs) DirectoryCacheKey(parent, leaf string) string {
	return actualID(parent) + "\x00" + f.opt.Enc.FromStandardName(leaf)
}

// Explicit API rejection and failed token acquisition cannot have created a
// directory. Transport errors and server failures require reconciliation.
func (f *Fs) AmbiguousDirectoryCreate(err error) bool {
	var api *googleapi.Error
	if errors.As(err, &api) {
		return api.Code >= 500 || api.Code == 408
	}
	var token *oauth2.RetrieveError
	if errors.As(err, &token) {
		return false
	}
	return true
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

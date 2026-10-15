// gclone-local VFS integration: copied from rclone v1.75.2; see vfs/UPSTREAM.txt.

// Package vfscache deals with caching of files locally for the VFS layer
package vfscache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscache/writeback"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	fscache "github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/lib/diskusage"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/file"
	"github.com/rclone/rclone/lib/systemd"
)

// NB as Cache and Item are tightly linked it is necessary to have a
// total lock ordering between them. So Cache.mu must always be
// taken before Item.mu to avoid deadlocks.
//
// Cache may call into Item but care is needed if Item calls Cache

// FIXME need to purge cache nodes which don't have backing files and aren't dirty
// these may get created by the VFS layer or may be orphans from reload()

// Cache opened files
type Cache struct {
	// read only - no locking needed to read these
	ctx        context.Context      // context for cache lifetime
	fremote    fs.Fs                // fs for the remote we are caching
	fcache     fs.Fs                // fs for the cache directory
	fcacheMeta fs.Fs                // fs for the cache metadata directory
	opt        *vfscommon.Options   // vfs Options
	root       string               // root of the cache directory
	metaRoot   string               // root of the cache metadata directory
	hashType   hash.Type            // hash to use locally and remotely
	hashOption *fs.HashesOption     // corresponding OpenOption
	writeback  *writeback.WriteBack // holds Items for writeback
	avFn       AddVirtualFn         // if set, can be called to add dir entries

	mu            sync.Mutex                // protects the following variables
	cond          sync.Cond                 // cond lock for synchronous cache cleaning
	item          map[string]*Item          // files/directories in the cache
	mutations     map[string]*pathMutation  // explicit unlink/rename pathname owners
	activities    map[string]*cacheActivity // initialization and maintenance owners
	epochs        map[string]uint64         // pathname binding generations
	bindingEpoch  uint64                    // advances on every map publication
	errOwners     map[string]cacheBinding   // generation which owns a reset error
	errItems      map[string]error          // items in error state
	used          int64                     // total size of files in the cache
	outOfSpace    bool                      // out of space
	cleanerKicked bool                      // some thread kicked the cleaner upon out of space
	kickerMu      sync.Mutex                // mutex for cleanerKicked
	kick          chan struct{}             // channel for kicking clear to start

}

// AddVirtualFn if registered by the WithAddVirtual method, can be
// called to register the object or directory at remote as a virtual
// entry in directory listings.
//
// This is used when reloading the Cache and uploading items need to
// go into the directory tree.
type AddVirtualFn func(remote string, size int64, isDir bool) error

// New creates a new cache hierarchy for fremote
//
// This starts background goroutines which can be cancelled with the
// context passed in.
func New(ctx context.Context, fremote fs.Fs, opt *vfscommon.Options, avFn AddVirtualFn) (*Cache, error) {
	// Get cache root path.
	// We need it in two variants: OS path as an absolute path with UNC prefix,
	// OS-specific path separators, and encoded with OS-specific encoder. Standard path
	// without UNC prefix, with slash path separators, and standard (internal) encoding.
	// Care must be taken when creating OS paths so that the ':' separator following a
	// drive letter is not encoded (e.g. into unicode fullwidth colon).
	var err error
	parentOSPath := config.GetCacheDir() // Assuming string contains a local absolute path in OS encoding
	fs.Debugf(fremote, "vfs cache: root is %q", parentOSPath)
	parentPath := fromOSPath(parentOSPath)

	// Get a relative cache path representing the remote.
	relativeDirPath := fremote.Root() // This is a remote path in standard encoding
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(relativeDirPath, `//?/`) {
			relativeDirPath = relativeDirPath[2:] // Trim off the "//" for the result to be a valid when appending to another path
		}
	}
	relativeDirPath = fremote.Name() + "/" + relativeDirPath
	relativeDirPath = clean(relativeDirPath)
	relativeDirOSPath := toOSPath(relativeDirPath)

	// Create cache root dirs
	var dataOSPath, metaOSPath string
	if dataOSPath, metaOSPath, err = createRootDirs(parentOSPath, relativeDirOSPath); err != nil {
		return nil, err
	}
	fs.Debugf(fremote, "vfs cache: data root is %q", dataOSPath)
	fs.Debugf(fremote, "vfs cache: metadata root is %q", metaOSPath)

	// Get (create) cache backends
	var fdata, fmeta fs.Fs
	if fdata, fmeta, err = getBackends(ctx, parentPath, relativeDirPath); err != nil {
		return nil, err
	}
	hashType, hashOption := operations.CommonHash(ctx, fdata, fremote)

	// Create the cache object
	c := &Cache{
		ctx:        ctx,
		fremote:    fremote,
		fcache:     fdata,
		fcacheMeta: fmeta,
		opt:        opt,
		root:       dataOSPath,
		metaRoot:   metaOSPath,
		item:       make(map[string]*Item),
		errItems:   make(map[string]error),
		hashType:   hashType,
		hashOption: hashOption,
		writeback:  writeback.New(ctx, opt),
		avFn:       avFn,
	}

	// load in the cache and metadata off disk
	err = c.reload(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load cache: %w", err)
	}

	// Remove any empty directories
	c.purgeEmptyDirs("", true)

	// Create a channel for cleaner to be kicked upon out of space con
	c.kick = make(chan struct{}, 1)
	c.cond = sync.Cond{L: &c.mu}

	go c.cleaner(ctx)

	return c, nil
}

// Stats returns info about the Cache
func (c *Cache) Stats() (out rc.Params) {
	out = make(rc.Params)
	// read only - no locking needed to read these
	out["path"] = c.root
	out["pathMeta"] = c.metaRoot
	out["hashType"] = c.hashType

	uploadsInProgress, uploadsQueued := c.writeback.Stats()
	out["uploadsInProgress"] = uploadsInProgress
	out["uploadsQueued"] = uploadsQueued

	c.mu.Lock()
	defer c.mu.Unlock()

	out["files"] = len(c.item)
	out["erroredFiles"] = len(c.errItems)
	out["bytesUsed"] = c.used
	out["outOfSpace"] = c.outOfSpace

	return out
}

// Queue returns info about the Cache
func (c *Cache) Queue() (out rc.Params) {
	out = make(rc.Params)
	out["queue"] = c.writeback.Queue()
	return out
}

// QueueSetExpiry updates the expiry of a single item in the upload queue
//
// The expiry time is set to expiry + relative if expiry is passed in,
// otherwise the expiry of the item is used.
func (c *Cache) QueueSetExpiry(id writeback.Handle, expiry time.Time, relative time.Duration) error {
	return c.writeback.SetExpiry(id, expiry, relative)
}

// createDir creates a directory path, along with any necessary parents
func createDir(dir string) error {
	return file.MkdirAll(dir, 0700)
}

// createRootDir creates a single cache root directory
func createRootDir(parentOSPath string, name string, relativeDirOSPath string) (path string, err error) {
	path = file.UNCPath(filepath.Join(parentOSPath, name, relativeDirOSPath))
	err = createDir(path)
	return
}

// createRootDirs creates all cache root directories
func createRootDirs(parentOSPath string, relativeDirOSPath string) (dataOSPath string, metaOSPath string, err error) {
	if dataOSPath, err = createRootDir(parentOSPath, "vfs", relativeDirOSPath); err != nil {
		err = fmt.Errorf("failed to create data cache directory: %w", err)
	} else if metaOSPath, err = createRootDir(parentOSPath, "vfsMeta", relativeDirOSPath); err != nil {
		err = fmt.Errorf("failed to create metadata cache directory: %w", err)
	}
	return
}

// createItemDir creates the directory for named item in all cache roots
//
// Returns an os path for the data cache file.
func (c *Cache) createItemDir(name string) (string, error) {
	parent := vfscommon.FindParent(name)
	parentPath := c.toOSPath(parent)
	err := createDir(parentPath)
	if err != nil {
		return "", fmt.Errorf("failed to create data cache item directory: %w", err)
	}
	parentPathMeta := c.toOSPathMeta(parent)
	err = createDir(parentPathMeta)
	if err != nil {
		return "", fmt.Errorf("failed to create metadata cache item directory: %w", err)
	}
	return c.toOSPath(name), nil
}

// getBackend gets a backend for a cache root dir
func getBackend(ctx context.Context, parentPath string, name string, relativeDirPath string) (fs.Fs, error) {
	// Make sure we turn off the global links flag as it overrides the backend specific one
	ctx, ci := fs.AddConfig(ctx)
	ci.Links = false
	path := fmt.Sprintf(":local,encoding='%v',links=false:%s/%s/%s", encoder.OS, parentPath, name, relativeDirPath)
	return fscache.Get(ctx, path)
}

// getBackends gets backends for all cache root dirs
func getBackends(ctx context.Context, parentPath string, relativeDirPath string) (fdata fs.Fs, fmeta fs.Fs, err error) {
	if fdata, err = getBackend(ctx, parentPath, "vfs", relativeDirPath); err != nil {
		err = fmt.Errorf("failed to get data cache backend: %w", err)
	} else if fmeta, err = getBackend(ctx, parentPath, "vfsMeta", relativeDirPath); err != nil {
		err = fmt.Errorf("failed to get metadata cache backend: %w", err)
	}
	return
}

// clean returns the cleaned version of name for use in the index map
//
// name should be a remote path not an osPath
func clean(name string) string {
	name = strings.Trim(name, "/")
	name = path.Clean(name)
	if name == "." || name == "/" {
		name = ""
	}
	return name
}

// fromOSPath turns a OS path into a standard/remote path
func fromOSPath(osPath string) string {
	return encoder.OS.ToStandardPath(filepath.ToSlash(osPath))
}

// toOSPath turns a standard/remote path into an OS path
func toOSPath(standardPath string) string {
	return filepath.FromSlash(encoder.OS.FromStandardPath(standardPath))
}

// toOSPath turns a remote relative name into an OS path in the cache
func (c *Cache) toOSPath(name string) string {
	return filepath.Join(c.root, toOSPath(name))
}

// toOSPathMeta turns a remote relative name into an OS path in the
// cache for the metadata
func (c *Cache) toOSPathMeta(name string) string {
	return filepath.Join(c.metaRoot, toOSPath(name))
}

// _get gets name from the cache or creates a new one
//
// It returns the item and found as to whether this item was found in
// the cache (or just created).
//
// name should be a remote path not an osPath
//
// must be called with mu held
// put registers an opened item without replacing another live lifetime.
//
// name should be a remote path not an osPath
func (c *Cache) put(name string, item *Item) error {
	name = clean(name)
	for {
		c.lockNames(name)
		oldItem := c.item[name]
		if !c.tryItems(item, oldItem) {
			continue
		}
		var err error
		if item.unlinked || item.unlinking {
			err = os.ErrNotExist
		} else if oldItem != nil && oldItem != item && (oldItem.opens != 0 || oldItem.unlinking || oldItem.unlinked) {
			err = fmt.Errorf("internal error: item %q already open in the cache", name)
		} else if oldItem != item {
			c.bindItem(name, item)
		}
		if oldItem != nil && oldItem != item {
			oldItem.mu.Unlock()
		}
		item.mu.Unlock()
		c.mu.Unlock()
		return err
	}
}

// InUse returns whether the name is in use in the cache
//
// name should be a remote path not an osPath
func (c *Cache) InUse(name string) bool {
	name = clean(name)
	c.lockNames(name)
	item := c.item[name]
	c.mu.Unlock()
	if item == nil {
		return false
	}
	return item.inUse()
}

// DirtyItem returns the Item if it exists in the cache **and** is
// dirty otherwise it returns nil.
//
// name should be a remote path not an osPath
func (c *Cache) DirtyItem(name string) (item *Item) {
	name = clean(name)
	for {
		c.lockNames(name)
		item = c.item[name]
		if item == nil {
			c.mu.Unlock()
			return nil
		}
		if !c.tryItems(item) {
			continue
		}
		dirty := item.info.Dirty && !item.unlinked && !item.unlinking
		item.mu.Unlock()
		c.mu.Unlock()
		if !dirty {
			return nil
		}
		return item
	}
}

// get gets a file name from the cache or creates a new one
//
// It returns the item and found as to whether this item was found in
// the cache (or just created).
//
// name should be a remote path not an osPath
func (c *Cache) get(name string) (item *Item, found bool) {
	name = clean(name)
	c.lockNames(name)
	if item = c.item[name]; item != nil {
		c.mu.Unlock()
		return item, true
	}
	activity := &cacheActivity{done: make(chan struct{})}
	if c.activities == nil {
		c.activities = make(map[string]*cacheActivity)
	}
	c.activities[name] = activity
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.activities, name)
		close(activity.done)
		c.mu.Unlock()
	}()
	item = newItem(c, name)
	c.mu.Lock()
	if current := c.item[name]; current != nil {
		item = current
		found = true
	} else {
		c.bindItem(name, item)
	}
	c.mu.Unlock()
	return item, found
}

// Item gets a cache item for name
//
// To use it item.Open will need to be called.
//
// name should be a remote path not an osPath
func (c *Cache) Item(name string) (item *Item) {
	item, _ = c.get(name)
	return item
}

// Exists checks to see if the file exists in the cache or not.
//
// This is done by bringing the item into the cache which will
// validate the backing file and metadata and then asking if the Item
// exists or not.
func (c *Cache) Exists(name string) bool {
	item, _ := c.get(name)
	return item.Exists()
}

// rename with os.Rename and more checking
func rename(osOldPath, osNewPath string) error {
	sfi, err := os.Stat(osOldPath)
	if err != nil {
		// Just do nothing if the source does not exist
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to stat source: %s: %w", osOldPath, err)
	}
	if !sfi.Mode().IsRegular() {
		// cannot copy non-regular files (e.g., directories, symlinks, devices, etc.)
		return fmt.Errorf("non-regular source file: %s (%q)", sfi.Name(), sfi.Mode().String())
	}
	dfi, err := os.Stat(osNewPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to stat destination: %s: %w", osNewPath, err)
		}
		parent := vfscommon.OSFindParent(osNewPath)
		err = createDir(parent)
		if err != nil {
			return fmt.Errorf("failed to create parent dir: %s: %w", parent, err)
		}
	} else {
		if !(dfi.Mode().IsRegular()) {
			return fmt.Errorf("non-regular destination file: %s (%q)", dfi.Name(), dfi.Mode().String())
		}
		if os.SameFile(sfi, dfi) {
			return nil
		}
	}
	if err = os.Rename(osOldPath, osNewPath); err != nil {
		return fmt.Errorf("failed to rename in cache: %s to %s: %w", osOldPath, osNewPath, err)
	}
	return nil
}

// Rename the item in cache
func (c *Cache) Rename(name string, newName string, newObj fs.Object) (err error) {
	mutation, err := c.BeginRename(name, newName)
	if err != nil {
		return err
	}
	defer mutation.Close()
	return mutation.Rename(newObj)
}

// rename requires a reservation covering both names.
func (c *Cache) rename(name string, newName string, newObj fs.Object) (err error) {
	item, _ := c.get(name)
	// Both names are reserved by this mutation. An initializer admitted
	// earlier must settle before either physical pathname can be replaced.
	c.lockNames(name, newName)
	c.mu.Unlock()
	err = item.rename(name, newName, newObj)
	if err != nil {
		return err
	}

	// Move the item in the cache
	c.mu.Lock()
	if item, ok := c.item[name]; ok {
		c.bindItem(newName, item)
		c.bindItem(name, nil)
		delete(c.errItems, name)
		delete(c.errOwners, name)
	}
	c.mu.Unlock()

	fs.Infof(name, "vfs cache: renamed in cache to %q", newName)
	return nil
}

// DirExists checks to see if the directory exists in the cache or not.
func (c *Cache) DirExists(name string) bool {
	path := c.toOSPath(name)
	_, err := os.Stat(path)
	return err == nil
}

// DirRename the dir in cache
func (c *Cache) DirRename(oldDirName string, newDirName string) (err error) {
	// Make sure names are / suffixed for reading keys out of c.item
	if !strings.HasSuffix(oldDirName, "/") {
		oldDirName += "/"
	}
	if !strings.HasSuffix(newDirName, "/") {
		newDirName += "/"
	}

	// Find all items to rename
	var renames []string
	c.mu.Lock()
	for itemName := range c.item {
		if strings.HasPrefix(itemName, oldDirName) {
			renames = append(renames, itemName)
		}
	}
	c.mu.Unlock()

	// Rename the items
	for _, itemName := range renames {
		newPath := newDirName + itemName[len(oldDirName):]
		renameErr := c.Rename(itemName, newPath, nil)
		if renameErr != nil {
			err = renameErr
		}
	}

	// Old path should be empty now so remove it
	c.purgeEmptyDirs(oldDirName[:len(oldDirName)-1], false)

	fs.Infof(oldDirName, "vfs cache: renamed dir in cache to %q", newDirName)
	return err
}

// Remove should be called if name is deleted
//
// This returns true if the file was in the transfer queue so may not
// have completely uploaded yet.
func (c *Cache) Remove(name string) (wasWriting bool) {
	name = clean(name)
	c.lockNames(name)
	// Legacy cache removal is advisory while an explicit namespace mutation
	// owns this name. Its owner already drains and retires the correct Item.
	if c.mutations[name] != nil {
		c.mu.Unlock()
		return false
	}
	item := c.item[name]
	if item != nil {
		c.bindItem(name, nil)
	}
	c.mu.Unlock()
	if item == nil {
		return false
	}
	return item.remove("file deleted")
}

// SetModTime should be called to set the modification time of the cache file
func (c *Cache) SetModTime(name string, modTime time.Time) {
	item, _ := c.get(name)
	item.setModTime(modTime)
}

// CleanUp empties the cache of everything
func (c *Cache) CleanUp() error {
	err1 := os.RemoveAll(c.root)
	err2 := os.RemoveAll(c.metaRoot)
	if err1 != nil {
		return err1
	}
	return err2
}

// walk walks the cache calling the function
func (c *Cache) walk(dir string, fn func(osPath string, fi os.FileInfo, name string) error) error {
	return filepath.Walk(dir, func(osPath string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Find path relative to the cache root
		name, err := filepath.Rel(dir, osPath)
		if err != nil {
			return fmt.Errorf("filepath.Rel failed in walk: %w", err)
		}
		if name == "." {
			name = ""
		}
		// And convert into slashes
		name = filepath.ToSlash(name)

		return fn(osPath, fi, name)
	})
}

// reload walks the cache loading metadata files
//
// It iterates the files first then metadata trees. It doesn't expect
// to find any new items iterating the metadata but it will clear up
// orphan files.
func (c *Cache) reload(ctx context.Context) error {
	for _, dir := range []string{c.root, c.metaRoot} {
		err := c.walk(dir, func(osPath string, fi os.FileInfo, name string) error {
			if fi.IsDir() {
				return nil
			}
			item, found := c.get(name)
			if !found {
				err := item.reload(ctx)
				if err != nil {
					fs.Errorf(name, "vfs cache: failed to reload item: %v", err)
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to walk cache %q: %w", dir, err)
		}
	}
	return nil
}

// KickCleaner kicks cache cleaner upon out of space situation
//
// This does nothing when the cleaner is disabled. Only the cleaner clears the
// out of space condition, so with no cleaner running the wait below would never
// return.
func (c *Cache) KickCleaner() {
	if c.opt.CachePollInterval <= 0 {
		return
	}

	/* Use a separate kicker mutex for the kick to go through without waiting for the
	   cache mutex to avoid letting a thread kick again after the clearer just
	   finished cleaning and unlock the cache mutex. */
	fs.Debugf(c.fremote, "vfs cache: at the beginning of KickCleaner")
	c.kickerMu.Lock()
	if !c.cleanerKicked {
		c.cleanerKicked = true
		fs.Debugf(c.fremote, "vfs cache: in KickCleaner, ready to lock cache mutex")
		c.mu.Lock()
		c.outOfSpace = true
		fs.Logf(c.fremote, "vfs cache: in KickCleaner, ready to kick cleaner")
		c.kick <- struct{}{}
		c.mu.Unlock()
	}
	c.kickerMu.Unlock()

	c.mu.Lock()
	for c.outOfSpace {
		fs.Debugf(c.fremote, "vfs cache: in KickCleaner, looping on c.outOfSpace")
		c.cond.Wait()
	}
	fs.Debugf(c.fremote, "vfs cache: in KickCleaner, leaving c.outOfSpace loop")
	c.mu.Unlock()
}

// removeNotInUse removes items not in use with a possible maxAge cutoff
// called with cache mutex locked and up-to-date c.used (as we update it directly here)
func (c *Cache) removeNotInUse(item *Item, maxAge time.Duration, emptyOnly bool) {
	for _, binding := range c.bindings() {
		if binding.item == item {
			c.evictBinding(binding, maxAge, emptyOnly)
			return
		}
	}
}

// Retry failed resets during purgeClean()
func (c *Cache) retryFailedResets() {
	c.mu.Lock()
	pending := make([]cacheBinding, 0, len(c.errOwners))
	for name, binding := range c.errOwners {
		if c.ownsBinding(binding) {
			pending = append(pending, binding)
		} else {
			delete(c.errOwners, name)
			delete(c.errItems, name)
		}
	}
	c.mu.Unlock()
	for _, binding := range pending {
		c.resetBinding(binding)
	}
	c.updateUsed()
}

// Remove cache files that are not dirty until the quota is satisfied
func (c *Cache) purgeClean() {
	used := c.updateUsed()
	candidates := c.maintenanceCandidates()
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].access.Before(candidates[j].access) })
	for _, candidate := range candidates {
		if c.quotaForSize(used) {
			break
		}
		used -= c.resetBinding(candidate.cacheBinding)
	}
	c.updateUsed()
	c.mu.Lock()
	c.outOfSpace = false
	c.cond.Broadcast()
	c.mu.Unlock()
}

// purgeOld gets rid of any files that are over age
func (c *Cache) purgeOld(maxAge time.Duration) {
	for _, binding := range c.bindings() {
		c.evictBinding(binding, maxAge, false)
	}
	c.updateUsed()
	if c.quotasOK() {
		c.mu.Lock()
		c.outOfSpace = false
		c.cond.Broadcast()
		c.mu.Unlock()
	}
}

// Purge any empty directories
func (c *Cache) purgeEmptyDirs(dir string, leaveRoot bool) {
	ctx := c.ctx
	err := operations.Rmdirs(ctx, c.fcache, dir, leaveRoot)
	if err != nil {
		fs.Errorf(c.fcache, "vfs cache: failed to remove empty directories from cache path %q: %v", dir, err)
	}
	err = operations.Rmdirs(ctx, c.fcacheMeta, dir, leaveRoot)
	if err != nil {
		fs.Errorf(c.fcache, "vfs cache: failed to remove empty directories from metadata cache path %q: %v", dir, err)
	}
}

// updateUsed updates c.used so it is accurate
func (c *Cache) updateUsed() (used int64) {
	for {
		c.mu.Lock()
		epoch := c.bindingEpoch
		bindings := make([]cacheBinding, 0, len(c.item))
		for name, item := range c.item {
			bindings = append(bindings, cacheBinding{name, item, c.epochs[name]})
		}
		c.mu.Unlock()
		used = 0
		for _, binding := range bindings {
			used += binding.item.getDiskSize()
		}
		c.mu.Lock()
		if c.bindingEpoch != epoch {
			c.mu.Unlock()
			continue
		}
		c.used = used
		c.mu.Unlock()
		return used
	}
}

// Check the available space for a disk is in limits.
func (c *Cache) minFreeSpaceQuotaOK() bool {
	if c.opt.CacheMinFreeSpace <= 0 {
		return true
	}
	du, err := diskusage.New(config.GetCacheDir())
	if err == diskusage.ErrUnsupported {
		return true
	}
	if err != nil {
		fs.Errorf(c.fremote, "disk usage returned error: %v", err)
		return true
	}
	return du.Available >= uint64(c.opt.CacheMinFreeSpace)
}

// Check the available quota for a disk is in limits.
//
// must be called with mu held.
func (c *Cache) maxSizeQuotaOK() bool {
	if c.opt.CacheMaxSize <= 0 {
		return true
	}
	return c.used <= int64(c.opt.CacheMaxSize)
}

// Check the available quotas for a disk is in limits.
//
// must be called with mu held.
func (c *Cache) quotasOK() bool {
	c.mu.Lock()
	used := c.used
	c.mu.Unlock()
	return c.quotaForSize(used)
}

// Return true if any quotas set
func (c *Cache) haveQuotas() bool {
	return c.opt.CacheMaxSize > 0 || c.opt.CacheMinFreeSpace > 0
}

// Remove clean cache files that are not open until the total space
// is reduced below quota starting from the oldest first
func (c *Cache) purgeOverQuota() {
	used := c.updateUsed()
	candidates := c.maintenanceCandidates()
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].access.Before(candidates[j].access) })
	for _, candidate := range candidates {
		used -= c.evictBinding(candidate.cacheBinding, 0, c.quotaForSize(used))
	}
	c.updateUsed()
	if c.quotasOK() {
		c.mu.Lock()
		c.outOfSpace = false
		c.cond.Broadcast()
		c.mu.Unlock()
	}
}

// clean empties the cache of stuff if it can
func (c *Cache) clean(kicked bool) {
	// Recover here rather than in the cleaner goroutine so that a panic
	// cleaning one item does not stop the cache being cleaned ever again.
	defer vfscommon.RecoverPanic(c.fremote, nil)
	// Cache may be empty so end
	_, err := os.Stat(c.root)
	if os.IsNotExist(err) {
		return
	}
	c.updateUsed()
	c.mu.Lock()
	oldItems, oldUsed := len(c.item), fs.SizeSuffix(c.used)
	c.mu.Unlock()

	// Remove any files that are over age
	c.purgeOld(time.Duration(c.opt.CacheMaxAge))

	// If have a maximum cache size...
	if c.haveQuotas() {
		// Remove files not in use until cache size is below quota starting from the oldest first
		c.purgeOverQuota()

		// Remove cache files that are not dirty if we are still above the max cache size
		c.purgeClean()
		c.retryFailedResets()
	}

	// Was kicked?
	if kicked {
		c.kickerMu.Lock() // Make sure this is called with cache mutex unlocked
		// Re-enable io threads to kick me
		c.cleanerKicked = false
		c.kickerMu.Unlock()
	}

	// Stats
	c.mu.Lock()
	newItems, newUsed := len(c.item), fs.SizeSuffix(c.used)
	c.mu.Unlock()
	totalInUse := c.TotalInUse()
	uploadsInProgress, uploadsQueued := c.writeback.Stats()

	stats := fmt.Sprintf("objects %d (was %d) in use %d, to upload %d, uploading %d, total size %v (was %v)",
		newItems, oldItems, totalInUse, uploadsQueued, uploadsInProgress, newUsed, oldUsed)
	fs.Infof(c.fremote, "vfs cache: cleaned: %s", stats)
	if err = systemd.UpdateStatus(fmt.Sprintf("[%s] vfs cache: %s", time.Now().Format("15:04"), stats)); err != nil {
		fs.Errorf(c.fremote, "vfs cache: updating systemd status with current stats failed: %s", err)
	}
}

// cleaner calls clean at regular intervals and upon being kicked for out-of-space condition
//
// doesn't return until context is cancelled
func (c *Cache) cleaner(ctx context.Context) {
	if c.opt.CachePollInterval <= 0 {
		fs.Debugf(c.fremote, "vfs cache: cleaning thread disabled because poll interval <= 0")
		return
	}
	// Start cleaning the cache immediately
	c.clean(false)
	// Then every interval specified
	timer := time.NewTicker(time.Duration(c.opt.CachePollInterval))
	defer timer.Stop()
	for {
		select {
		case <-c.kick: // a thread encountering ENOSPC kicked me
			c.clean(true) // kicked is true
		case <-timer.C:
			c.clean(false) // timer driven cache poll, kicked is false
		case <-ctx.Done():
			fs.Debugf(c.fremote, "vfs cache: cleaner exiting")
			return
		}
	}
}

// TotalInUse returns the number of items in the cache which are InUse
func (c *Cache) TotalInUse() (n int) {
	for _, binding := range c.bindings() {
		if binding.item.inUse() {
			n++
		}
	}
	return n
}

// Dump the cache into a string for debugging purposes
func (c *Cache) Dump() string {
	if c == nil {
		return "Cache: <nil>\n"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out strings.Builder
	out.WriteString("Cache{\n")
	for name, item := range c.item {
		fmt.Fprintf(&out, "\t%q: %+v,\n", name, item)
	}
	out.WriteString("}\n")
	return out.String()
}

// AddVirtual adds a virtual directory entry by calling the addVirtual
// callback if one has been registered.
func (c *Cache) AddVirtual(remote string, size int64, isDir bool) error {
	if c.avFn == nil {
		return errors.New("no AddVirtual function registered")
	}
	return c.avFn(remote, size, isDir)
}

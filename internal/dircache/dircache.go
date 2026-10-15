// Directory ID cache adapted from rclone v1.75.2; see UPSTREAM.txt.
// Remote resolution is coalesced by pathname, never by a network-length global lock.
package dircache

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
)

type DirCacher interface {
	FindLeaf(context.Context, string, string) (string, bool, error)
	CreateDir(context.Context, string, string) (string, error)
}

type CreateFn func(context.Context, string, string) (string, error)

type parentBinding struct {
	path, id string
	epoch    uint64
}
type flightKey struct {
	path                   string
	parentID               string
	parentEpoch, rootEpoch uint64
}
type lookupFlight struct {
	done                  chan struct{}
	initialCreate, create bool
	creator               CreateFn
	id                    string
	epoch                 uint64
	err                   error
}
type creationLease struct {
	key       string
	done      chan struct{}
	id        string
	err       error
	ambiguous bool
	scope     resolutionScope
}
type resolutionScope struct {
	root, epoch uint64
	parent      string
	relative    bool
}
type rootFlight struct {
	done                  chan struct{}
	initialCreate, create bool
	epoch                 uint64
	err                   error
}

// DirCache protects only bookkeeping with mu. A physical creation lease stays
// alive across logical invalidation until a valid namespace accepts its result.
type DirCache struct {
	mu                                     sync.Mutex
	fs                                     DirCacher
	ctx                                    context.Context
	root, trueRootID, rootID, rootParentID string
	foundRoot                              bool
	cache, invCache                        map[string]string
	epochs                                 map[string]uint64
	sequence, rootEpoch                    uint64
	flights                                map[flightKey]*lookupFlight
	creations                              map[string]*creationLease
	physical                               map[string]chan struct{}
	rootFlight                             *rootFlight
	slots                                  chan struct{}
}

var errObservationChanged = errors.New("directory cache observation changed")

func New(root, trueRootID string, remote DirCacher) *DirCache {
	return NewWithContext(context.Background(), root, trueRootID, remote)
}

func NewWithContext(ctx context.Context, root, trueRootID string, remote DirCacher) *DirCache {
	parallel := fs.GetConfig(ctx).Checkers
	if parallel < 1 {
		parallel = 1
	}
	if parallel > 16 {
		parallel = 16
	}
	dc := &DirCache{fs: remote, ctx: ctx, root: root, trueRootID: trueRootID,
		flights: make(map[flightKey]*lookupFlight), creations: make(map[string]*creationLease), physical: make(map[string]chan struct{}), slots: make(chan struct{}, parallel)}
	dc.ResetRoot()
	return dc
}

func (dc *DirCache) String() string {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	return fmt.Sprintf("DirCache{root:%q rootID:%q found:%v cache:%v}", dc.root, dc.rootID, dc.foundRoot, dc.cache)
}

func (dc *DirCache) Get(name string) (string, bool) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	id, ok := dc.cache[name]
	return id, ok
}
func (dc *DirCache) GetInv(id string) (string, bool) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	name, ok := dc.invCache[id]
	return name, ok
}

func within(name, prefix string) bool {
	return prefix == "" || name == prefix || strings.HasPrefix(name, prefix+"/")
}

func (dc *DirCache) eraseLocked(name string) {
	id := dc.cache[name]
	if dc.invCache[id] == name {
		delete(dc.invCache, id)
	}
	delete(dc.cache, name)
	delete(dc.epochs, name)
}

func (dc *DirCache) putLocked(name, id string) uint64 {
	if dc.cache[name] == id && dc.epochs[name] != 0 {
		return dc.epochs[name]
	}
	if previous, ok := dc.cache[name]; ok && previous != id {
		for child := range dc.cache {
			if within(child, name) {
				dc.eraseLocked(child)
			}
		}
	}
	dc.sequence++
	dc.cache[name] = id
	dc.invCache[id] = name
	dc.epochs[name] = dc.sequence
	return dc.sequence
}

func (dc *DirCache) Put(name, id string) { dc.mu.Lock(); defer dc.mu.Unlock(); dc.putLocked(name, id) }

func (dc *DirCache) flushLocked() {
	dc.sequence++
	dc.rootEpoch = dc.sequence
	dc.cache = make(map[string]string)
	dc.invCache = make(map[string]string)
	dc.epochs = make(map[string]uint64)
}
func (dc *DirCache) Flush() { dc.mu.Lock(); defer dc.mu.Unlock(); dc.flushLocked() }
func (dc *DirCache) ResetRoot() {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.flushLocked()
	dc.foundRoot = false
	dc.rootID = dc.trueRootID
	dc.rootParentID = ""
	dc.putLocked("", dc.trueRootID)
}
func (dc *DirCache) FlushDir(name string) {
	if name == "" {
		dc.ResetRoot()
		return
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	for child := range dc.cache {
		if within(child, name) {
			dc.eraseLocked(child)
		}
	}
	// An older listing of the parent must not put the removed child back.
	parent, _ := SplitPath(name)
	if _, ok := dc.cache[parent]; ok {
		dc.sequence++
		dc.epochs[parent] = dc.sequence
	}
}
func (dc *DirCache) SetRootIDAlias(id string) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.rootID = id
	dc.putLocked("", id)
}

func SplitPath(name string) (directory, leaf string) {
	index := strings.LastIndex(name, "/")
	if index < 0 {
		return "", name
	}
	return name[:index], name[index+1:]
}

func (dc *DirCache) key(parent, leaf string) string {
	if remote, ok := dc.fs.(interface{ DirectoryCacheKey(string, string) string }); ok {
		return remote.DirectoryCacheKey(parent, leaf)
	}
	return parent + "\x00" + leaf
}

func (dc *DirCache) wait(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-dc.ctx.Done():
		return dc.ctx.Err()
	}
}

// A canceled waiter does not cancel another waiter's physical operation.
// Cache shutdown still cancels all workers through the construction context.
func (dc *DirCache) workerContext(ctx context.Context) (context.Context, func()) {
	worker, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(dc.ctx, cancel)
	if dc.ctx.Err() != nil {
		cancel()
	}
	return worker, func() { stop(); cancel() }
}

func (dc *DirCache) admitted(ctx context.Context, operation, parent, leaf string, call func() error) (err error) {
	defer func() {
		if caught := recover(); caught != nil {
			err = fmt.Errorf("directory backend panic: %v", caught)
		}
	}()
	queued := time.Now()
	if err = ctx.Err(); err != nil {
		return err
	}
	select {
	case dc.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	started := time.Now()
	defer func() {
		elapsed := time.Since(started)
		<-dc.slots
		// Backend time includes its pacer and authentication; it is not a
		// claim about the server-only HTTP execution time.
		fs.Debugf(nil, "directory resolver %s parent=%q leaf=%q admission=%v backend=%v err=%v", operation, parent, leaf, started.Sub(queued), elapsed, err)
	}()
	if err = ctx.Err(); err != nil {
		return err
	}
	return call()
}

func (dc *DirCache) findLeaf(ctx context.Context, parent, leaf string) (id string, found bool, err error) {
	err = dc.admitted(ctx, "lookup", parent, leaf, func() error { var e error; id, found, e = dc.fs.FindLeaf(ctx, parent, leaf); return e })
	return
}

func (dc *DirCache) leaseResult(ctx context.Context, lease *creationLease, parent, leaf string, scope resolutionScope) (string, error) {
	if err := dc.wait(ctx, lease.done); err != nil {
		return "", err
	}
	dc.mu.Lock()
	id, err, ambiguous, original := lease.id, lease.err, lease.ambiguous, lease.scope
	dc.mu.Unlock()
	if err == nil && scope == original {
		return id, nil
	}
	if err != nil && !ambiguous {
		return "", err
	}
	// An ambiguous create is reconciled rather than blindly issued again.
	fresh, found, lookupErr := dc.findLeaf(ctx, parent, leaf)
	if lookupErr != nil {
		return "", lookupErr
	}
	if !found {
		if err == nil {
			err = errors.New("created ID is not observable after invalidation")
		}
		return "", fmt.Errorf("directory creation outcome unresolved for %q: %w", leaf, err)
	}
	dc.mu.Lock()
	lease.id, lease.err = fresh, nil
	dc.mu.Unlock()
	return fresh, nil
}

func (dc *DirCache) resolvePhysical(ctx context.Context, parent, leaf string, create bool, creator CreateFn, scope resolutionScope) (string, *creationLease, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	key := dc.key(parent, leaf)
	// Serialize creation-capable observation/create transactions across aliases.
	// Otherwise an older absent lookup could authorize another create after a
	// different alias has already published and retired its creation lease.
	if create {
		for {
			dc.mu.Lock()
			active := dc.physical[key]
			if active == nil {
				active = make(chan struct{})
				dc.physical[key] = active
				dc.mu.Unlock()
				defer func() {
					dc.mu.Lock()
					delete(dc.physical, key)
					close(active)
					dc.mu.Unlock()
				}()
				break
			}
			dc.mu.Unlock()
			if err := dc.wait(ctx, active); err != nil {
				return "", nil, err
			}
		}
	}
	dc.mu.Lock()
	lease := dc.creations[key]
	dc.mu.Unlock()
	if lease != nil {
		id, err := dc.leaseResult(ctx, lease, parent, leaf, scope)
		return id, lease, err
	}
	id, found, err := dc.findLeaf(ctx, parent, leaf)
	if err != nil {
		return "", nil, err
	}
	if found {
		return id, nil, nil
	}
	if !create {
		return "", nil, fs.ErrorDirNotFound
	}
	dc.mu.Lock()
	if dc.rootEpoch != scope.root || (scope.relative && (dc.cache[scope.parent] != parent || dc.epochs[scope.parent] != scope.epoch)) {
		dc.mu.Unlock()
		return "", nil, errObservationChanged
	}
	lease = dc.creations[key]
	if lease == nil {
		// Unsettled attempts cannot be evicted safely. Bound their retained state
		// rather than trading an unknown outcome for duplicate remote creations.
		if len(dc.creations) >= 1024 {
			dc.mu.Unlock()
			return "", nil, errors.New("too many unresolved directory creations")
		}
		lease = &creationLease{key: key, done: make(chan struct{}), scope: scope}
		dc.creations[key] = lease
		worker, finish := dc.workerContext(ctx)
		if creator == nil {
			creator = dc.fs.CreateDir
		}
		go func() {
			defer finish()
			var result string
			createErr := dc.admitted(worker, "create", parent, leaf, func() error { var e error; result, e = creator(worker, parent, leaf); return e })
			dc.mu.Lock()
			defer dc.mu.Unlock()
			lease.id, lease.err, lease.ambiguous = result, createErr, createErr != nil
			if createErr != nil {
				if classifier, ok := dc.fs.(interface{ AmbiguousDirectoryCreate(error) bool }); ok && !classifier.AmbiguousDirectoryCreate(createErr) {
					lease.ambiguous = false
					delete(dc.creations, key)
				}
			}
			close(lease.done)
		}()
	}
	dc.mu.Unlock()
	id, err = dc.leaseResult(ctx, lease, parent, leaf, scope)
	return id, lease, err
}

func (dc *DirCache) settleLocked(lease *creationLease, id string) {
	if lease != nil && lease.id == id && lease.err == nil && dc.creations[lease.key] == lease {
		delete(dc.creations, lease.key)
	}
}

func (dc *DirCache) FindDir(ctx context.Context, name string, create bool) (string, error) {
	return dc.findDir(ctx, name, create, nil)
}

// FindDirWithCreate shares creation ownership with ordinary Mkdir. A caller
// whose creator did not run may apply its own metadata after resolving the ID.
func (dc *DirCache) FindDirWithCreate(ctx context.Context, name string, creator CreateFn) (string, error) {
	return dc.findDir(ctx, name, true, creator)
}

func (dc *DirCache) findDir(ctx context.Context, name string, create bool, creator CreateFn) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := dc.FindRoot(ctx, create); err != nil {
			return "", err
		}
		dc.mu.Lock()
		if !dc.foundRoot {
			dc.mu.Unlock()
			continue
		}
		if name == "" {
			id := dc.rootID
			dc.mu.Unlock()
			return id, nil
		}
		if id, ok := dc.cache[name]; ok {
			dc.mu.Unlock()
			return id, nil
		}
		dc.mu.Unlock()
		parent, leaf := SplitPath(name)
		parentID, err := dc.findDir(ctx, parent, create, nil)
		if err != nil {
			return "", err
		}
		dc.mu.Lock()
		if !dc.foundRoot || dc.cache[parent] != parentID {
			dc.mu.Unlock()
			continue
		}
		if id, ok := dc.cache[name]; ok {
			dc.mu.Unlock()
			return id, nil
		}
		key := flightKey{name, parentID, dc.epochs[parent], dc.rootEpoch}
		flight := dc.flights[key]
		if flight == nil {
			flight = &lookupFlight{done: make(chan struct{}), create: create, initialCreate: create, creator: creator}
			dc.flights[key] = flight
			worker, finish := dc.workerContext(ctx)
			go func() {
				defer finish()
				dc.mu.Lock()
				mode, makeDir := flight.create, flight.creator
				dc.mu.Unlock()
				id, lease, err := dc.resolvePhysical(worker, parentID, leaf, mode, makeDir, resolutionScope{key.rootEpoch, key.parentEpoch, parent, true})
				dc.mu.Lock()
				defer dc.mu.Unlock()
				if err == nil {
					if dc.rootEpoch != key.rootEpoch || dc.cache[parent] != parentID || dc.epochs[parent] != key.parentEpoch {
						err = errObservationChanged
					} else if current, ok := dc.cache[name]; ok && current != id {
						err = errObservationChanged
					} else {
						flight.epoch = dc.putLocked(name, id)
						dc.settleLocked(lease, id)
					}
				}
				flight.id, flight.err = id, err
				delete(dc.flights, key)
				close(flight.done)
			}()
		} else if create {
			flight.create = true
			if flight.creator == nil {
				flight.creator = creator
			}
		}
		dc.mu.Unlock()
		if err := dc.wait(ctx, flight.done); err != nil {
			return "", err
		}
		dc.mu.Lock()
		id, err, epoch := flight.id, flight.err, flight.epoch
		current := dc.cache[name] == id && dc.epochs[name] == epoch && dc.rootEpoch == key.rootEpoch
		dc.mu.Unlock()
		if errors.Is(err, errObservationChanged) || (err == nil && !current) {
			continue
		}
		if errors.Is(err, fs.ErrorDirNotFound) && create && !flight.initialCreate {
			continue
		}
		return id, err
	}
}

func (dc *DirCache) absolute(ctx context.Context, name string, create bool, epoch uint64) (id, parent string, err error) {
	dc.mu.Lock()
	id = dc.trueRootID
	dc.mu.Unlock()
	if name == "" {
		return id, "", nil
	}
	prefix := ""
	for _, leaf := range strings.Split(name, "/") {
		parent = id
		var lease *creationLease
		id, lease, err = dc.resolvePhysical(ctx, parent, leaf, create, nil, resolutionScope{root: epoch, epoch: epoch})
		if err != nil {
			return "", "", err
		}
		prefix = path.Join(prefix, leaf)
		dc.mu.Lock()
		valid := dc.rootEpoch == epoch
		if valid && !dc.foundRoot {
			dc.putLocked(prefix, id)
			dc.settleLocked(lease, id)
		}
		dc.mu.Unlock()
		if !valid {
			return "", "", errObservationChanged
		}
	}
	return id, parent, nil
}

func (dc *DirCache) FindRoot(ctx context.Context, create bool) error {
	for {
		dc.mu.Lock()
		if dc.foundRoot {
			if dc.cache[""] == "" {
				dc.putLocked("", dc.rootID)
			}
			dc.mu.Unlock()
			return nil
		}
		if dc.root == "" {
			dc.foundRoot = true
			dc.rootID = dc.trueRootID
			dc.putLocked("", dc.rootID)
			dc.mu.Unlock()
			return nil
		}
		flight := dc.rootFlight
		if flight == nil || flight.epoch != dc.rootEpoch {
			flight = &rootFlight{done: make(chan struct{}), create: create, initialCreate: create, epoch: dc.rootEpoch}
			dc.rootFlight = flight
			worker, finish := dc.workerContext(ctx)
			go func() {
				defer finish()
				dc.mu.Lock()
				mode := flight.create
				dc.mu.Unlock()
				id, parent, err := dc.absolute(worker, dc.root, mode, flight.epoch)
				dc.mu.Lock()
				defer dc.mu.Unlock()
				if err == nil {
					if dc.rootEpoch != flight.epoch {
						err = errObservationChanged
					} else {
						dc.cache = make(map[string]string)
						dc.invCache = make(map[string]string)
						dc.epochs = make(map[string]uint64)
						dc.rootID, dc.rootParentID, dc.foundRoot = id, parent, true
						dc.putLocked("", id)
					}
				}
				flight.err = err
				if dc.rootFlight == flight {
					dc.rootFlight = nil
				}
				close(flight.done)
			}()
		} else if create {
			flight.create = true
		}
		dc.mu.Unlock()
		if err := dc.wait(ctx, flight.done); err != nil {
			return err
		}
		dc.mu.Lock()
		err := flight.err
		dc.mu.Unlock()
		if errors.Is(err, errObservationChanged) || (errors.Is(err, fs.ErrorDirNotFound) && create && !flight.initialCreate) {
			continue
		}
		if err != nil {
			return err
		}
	}
}

func (dc *DirCache) FoundRoot() bool { dc.mu.Lock(); defer dc.mu.Unlock(); return dc.foundRoot }
func (dc *DirCache) RootID(ctx context.Context, create bool) (string, error) {
	return dc.FindDir(ctx, "", create)
}
func (dc *DirCache) RootParentID(ctx context.Context, create bool) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		dc.mu.Lock()
		if dc.root == "" || (dc.foundRoot && dc.rootID == dc.trueRootID) {
			dc.mu.Unlock()
			return "", errors.New("is root directory")
		}
		if dc.foundRoot {
			id := dc.rootParentID
			dc.mu.Unlock()
			return id, nil
		}
		epoch := dc.rootEpoch
		parent, _ := SplitPath(dc.root)
		dc.mu.Unlock()
		id, _, err := dc.absolute(ctx, parent, create, epoch)
		if errors.Is(err, errObservationChanged) {
			continue
		}
		return id, err
	}
}
func (dc *DirCache) FindPath(ctx context.Context, name string, create bool) (leaf, id string, err error) {
	if name == "" {
		_, leaf = SplitPath(dc.root)
		id, err = dc.RootParentID(ctx, create)
	} else {
		var parent string
		parent, leaf = SplitPath(name)
		id, err = dc.FindDir(ctx, parent, create)
	}
	return
}

type observationKey struct{}
type observation struct {
	cache               *DirCache
	rootEpoch, sequence uint64
	parents             map[string]parentBinding
}

// Observe ties listing publications to the parent bindings used by that HTTP
// request. A fresh generation sampled only when Put runs would accept old data.
func (dc *DirCache) Observe(ctx context.Context, parents map[string]string) context.Context {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	token := &observation{cache: dc, rootEpoch: dc.rootEpoch, sequence: dc.sequence, parents: make(map[string]parentBinding)}
	for name, id := range parents {
		if current := dc.cache[name]; current != "" && dc.key(current, "") == dc.key(id, "") {
			token.parents[name] = parentBinding{name, current, dc.epochs[name]}
		}
	}
	return context.WithValue(ctx, observationKey{}, token)
}

func (dc *DirCache) PutContext(ctx context.Context, name, id string) bool {
	token, _ := ctx.Value(observationKey{}).(*observation)
	if token == nil || token.cache != dc {
		dc.Put(name, id)
		return true
	}
	parent, _ := SplitPath(name)
	dc.mu.Lock()
	defer dc.mu.Unlock()
	expected, ok := token.parents[parent]
	if !ok || token.rootEpoch != dc.rootEpoch || dc.cache[parent] != expected.id || dc.epochs[parent] != expected.epoch {
		return false
	}
	if dc.epochs[name] > token.sequence && dc.cache[name] != id {
		return false
	}
	dc.putLocked(name, id)
	return true
}

// DirMove prepares to move the directory (srcDC, srcRoot, srcRemote)
// into the directory (dc, dstRoot, dstRemote)
//
// It does all the checking, creates intermediate directories and
// returns leafs and IDs ready for the move.
//
// This returns:
//
// - srcID - ID of the source directory
// - srcDirectoryID - ID of the parent of the source directory
// - srcLeaf - leaf name of the source directory
// - dstDirectoryID - ID of the parent of the destination directory
// - dstLeaf - leaf name of the destination directory
//
// These should be used to do the actual move then
// srcDC.FlushDir(srcRemote) should be called.
func (dc *DirCache) DirMove(
	ctx context.Context, srcDC *DirCache, srcRoot, srcRemote, dstRoot, dstRemote string) (srcID, srcDirectoryID, srcLeaf, dstDirectoryID, dstLeaf string, err error) {
	var (
		dstDC   = dc
		srcPath = path.Join(srcRoot, srcRemote)
		dstPath = path.Join(dstRoot, dstRemote)
	)

	// Refuse to move to or from the root
	if srcPath == "" || dstPath == "" {
		// fs.Debugf(src, "DirMove error: Can't move root")
		err = errors.New("can't move root directory")
		return
	}

	// Find ID of dst parent, creating subdirs if necessary
	dstLeaf, dstDirectoryID, err = dstDC.FindPath(ctx, dstRemote, true)
	if err != nil {
		return
	}

	// Check destination does not exist
	_, err = dstDC.FindDir(ctx, dstRemote, false)
	if err == fs.ErrorDirNotFound {
		// OK
	} else if err != nil {
		return
	} else {
		err = fs.ErrorDirExists
		return
	}

	// Find ID of src parent
	srcLeaf, srcDirectoryID, err = srcDC.FindPath(ctx, srcRemote, false)
	if err != nil {
		return
	}

	// Find ID of src
	srcID, err = srcDC.FindDir(ctx, srcRemote, false)
	if err != nil {
		return
	}

	return
}

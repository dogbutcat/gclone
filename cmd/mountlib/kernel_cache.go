// Retire kernel data views independently of the request that changed the data.
package mountlib

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/dogbutcat/gclone/vfs"
	"github.com/dogbutcat/gclone/vfs/vfscache"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
)

func UsesKernelCacheControl(view *vfs.VFS, opt *Options) bool {
	return runtime.GOOS == "linux" && view.Opt.CacheMode == vfscommon.CacheModeFull && !opt.DirectIO && !opt.WritebackCache
}

// KernelCache owns one canonical inode's retirement queue. A queued change
// survives a cancelled OPEN or a failed notification; already-open readers do
// not have to reopen to trigger delivery. No VFS lock is held while notifying.
type KernelCache struct {
	owner                *vfscache.KernelCacheOwner
	ctx                  context.Context
	cancel               context.CancelFunc
	mu                   sync.Mutex
	notify               func() error
	requested, completed uint64
	running              bool
	lastError            error
	wake                 chan struct{}
}

func NewKernelCache() *KernelCache { return NewKernelCacheWithContext(context.Background()) }

func NewKernelCacheWithContext(parent context.Context) *KernelCache {
	ctx, cancel := context.WithCancel(parent)
	cache := &KernelCache{ctx: ctx, cancel: cancel, wake: make(chan struct{})}
	cache.owner = vfscache.NewKernelCacheOwnerWithInvalidation(cache.changed, cache.Wait)
	return cache
}

func (cache *KernelCache) Owner() *vfscache.KernelCacheOwner { return cache.owner }
func (cache *KernelCache) Close()                            { cache.cancel() }

func (cache *KernelCache) signal() { close(cache.wake); cache.wake = make(chan struct{}) }

func (cache *KernelCache) start() {
	if !cache.running && cache.notify != nil && cache.completed != cache.requested && cache.ctx.Err() == nil {
		cache.running = true
		go cache.deliver()
	}
}

func (cache *KernelCache) changed() {
	cache.mu.Lock()
	cache.requested++
	cache.lastError = nil
	cache.start()
	cache.mu.Unlock()
}

// SetNotifier binds the kernel node before its first accepted OPEN. Tests may
// replace it while quiescent to exercise failure and completion boundaries.
func (cache *KernelCache) SetNotifier(notify func() error) {
	cache.mu.Lock()
	cache.notify = notify
	cache.start()
	cache.mu.Unlock()
}

func (cache *KernelCache) deliver() {
	for {
		cache.mu.Lock()
		if cache.ctx.Err() != nil || cache.completed == cache.requested {
			cache.running = false
			cache.signal()
			cache.mu.Unlock()
			return
		}
		serial, notify := cache.requested, cache.notify
		cache.mu.Unlock()
		err := notify()
		cache.mu.Lock()
		cache.lastError = err
		if err == nil {
			cache.completed = serial
		}
		cache.signal()
		cache.mu.Unlock()
		if err != nil {
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-timer.C:
			case <-cache.ctx.Done():
				timer.Stop()
			}
		}
	}
}

// Wait acknowledges only completed invalidations. Returning an error does not
// cancel the queue: transient notification failures retry without another OPEN.
func (cache *KernelCache) Wait(ctx context.Context) error {
	for {
		cache.mu.Lock()
		if err := ctx.Err(); err != nil {
			cache.mu.Unlock()
			return err
		}
		if err := cache.ctx.Err(); err != nil {
			cache.mu.Unlock()
			return nil // the mount is gone, so it cannot retain kernel pages
		}
		if cache.completed == cache.requested {
			cache.mu.Unlock()
			return nil
		}
		// Before the first OPEN acknowledgement this canonical node has no
		// cached kernel data. Retain the queued serial for Prepare, but do not
		// stall another VFS user on an observer which has not been attached yet.
		if cache.notify == nil {
			cache.mu.Unlock()
			return nil
		}
		if err := cache.lastError; err != nil {
			cache.mu.Unlock()
			return err
		}
		wake := cache.wake
		cache.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		case <-cache.ctx.Done():
			return nil // the kernel view disappeared with its mount
		}
	}
}

func (cache *KernelCache) Prepare(ctx context.Context, handle vfs.Handle, notify func() error) (bool, error) {
	cached, ok := handle.(*vfs.RWFileHandle)
	if !ok {
		return false, nil
	}
	_, eligible, err := cached.KernelCacheVersion()
	if err != nil || !eligible {
		return false, err
	}
	cache.mu.Lock()
	if cache.notify == nil {
		cache.notify = notify
	}
	cache.start()
	cache.mu.Unlock()
	if err := cached.SyncKernelViews(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// Keep remote revalidation outside cache-item locks without accepting stale results.
package vfscache

import (
	"context"

	"github.com/rclone/rclone/fs"
)

// _lookupObject requires Item.mu and returns with it held. The lookup pins this
// cache lifetime, but status/namespace readers can still inspect it immediately.
// Reset never needs network access: guarded items are left for a foreground open.
func (item *Item) _lookupObject(ctx context.Context) (o fs.Object, err error) {
	if item.beingReset || item.sourceChanging || item.pendingWrites != 0 {
		return nil, errObjectViewChanged
	}
	obs := item._objectObservation()
	dirty, publishing, revalidate := item.info.Dirty, item.publishing, item.mustRevalidate
	fd, closing, downloadEpoch := item.fd, item.closing, item.downloadEpoch
	item.pendingLookups++
	defer func() {
		item.pendingLookups--
		item.cond.Broadcast()
	}()
	unlockMutexForCall(&item.mu, func() {
		o, err = item.c.fremote.NewObject(ctx, obs.name)
	})
	// Check the request's original owner BEFORE interpreting errors/absence or
	// rebasing the token for mutations made by _checkObjectView itself. Callback
	// completion changes publishing/Dirty even when the object epoch stays put.
	if !item._ownsObservation(obs) || item.info.Dirty != dirty || item.publishing != publishing ||
		item.mustRevalidate != revalidate || item.fd != fd || item.closing != closing ||
		item.downloadEpoch != downloadEpoch || item.beingReset || item.sourceChanging {
		return nil, errObjectViewChanged
	}
	return o, err
}
